// Package cadence decides how long to wait before the next sweep (issue #1).
//
// tanktemp sweeps slowly by default. When any probe changes quickly (a
// hot-water draw: the cold inlet falls and the outlet rises) it jumps
// straight to a fast interval, holds it briefly once the changes stop, then
// eases back to the slow interval. Any new change jumps straight back to
// fast. The point is to catch a draw's start, end and flow rate, which need
// fast sampling, without paying for fast sampling through the long, quiet
// overnight cooling.
//
// It is core logic, as pure as sweep: no os, no clock, no MQTT, no 1-Wire.
// It sees time only as it's handed in: each sweep's start on the monotonic
// clock (lifecycle's start time for that unit of work), which times the
// rates and the curve, and Sweep.At, the wall-clock timestamp, which only
// labels the trigger it reports. So a wall-clock step (NTP at boot, on a Pi
// with no real-time clock) can't move the cadence (review on #4; this
// retired GUESSES G45). Its only state is the previous sweep's readings,
// the curve's anchor and the last trigger. That makes it a table test, with
// no fake clock at all.
//
// The rules (the issue's numbering):
//
//  1. The trigger is a rate, |ΔT| / Δt, between a probe's reading in the
//     previous sweep and in this one, on every probe of every tank. A rate
//     rather than a step, so it means the same at either interval. The
//     change must also be at least MinChangeC, two DS18B20 steps, so a
//     one-step flicker between fast sweeps never triggers.
//  2. Only OK readings count: a probe that failed, has no OK reading in the
//     sweep before (the first sweep, or it just recovered), or reads a
//     suspect 85 °C on either side, can't trigger.
//  3. The interval depends only on t, the time since the last trigger:
//     fast during the hold, an exponential ease from fast to slow during
//     the decay, and slow after (or before any trigger). See Settings.at.
//  4. The next sweep is due one interval after the previous one was due,
//     and an overrun skips ticks rather than queueing them. See Schedule
//     and NextDue; lifecycle's Config.Next calls them.
//
// What it decides is never published: the MQTT payloads carry no cadence
// (docs/WIRE.md, Cadence). It goes to /api/status and, with log.verbose,
// the journal.
package cadence

import (
	"fmt"
	"math"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// Mode is where the cadence is on its curve. The strings are what
// /api/status reports as sweep.mode.
type Mode string

const (
	// Slow: no trigger yet, or the last one is more than hold + decay ago.
	// The interval is Settings.Slow. With fast off, it's always Slow.
	Slow Mode = "slow"
	// Hold: less than hold since the last trigger. The interval is
	// Settings.Fast. A trigger always lands here.
	Hold Mode = "hold"
	// Decay: easing from Fast back to Slow.
	Decay Mode = "decay"
)

// MinChangeC is the smallest change in °C that can trigger, whatever its
// rate: two DS18B20 steps of 1/16 °C. One step between 10 s sweeps is
// 0.375 °C/min, already under a sensible trigger, but at a 2 s fast
// interval it's 1.875 °C/min, over the default 1.5. Two steps can't be a
// flicker between two adjacent register values. Raw values are exact
// sixteenths, so comparing against it is exact. It's a constant, not a
// config key, because nothing about a rig changes the sensor's step size
// (GUESSES G40).
const MinChangeC = 2 * 0.0625

// Settings is the cadence's configuration, from config.Sweep.
type Settings struct {
	Slow           time.Duration // sweep.interval: the longest gap
	Fast           time.Duration // sweep.fast.interval; 0 means fast is off
	TriggerCPerMin float64       // sweep.fast.trigger_c_per_min
	Hold           time.Duration // sweep.fast.hold
	Decay          time.Duration // sweep.fast.decay
}

// FromConfig takes the sweep settings as config.Load returns them, already
// validated. With no sweep.fast block, Fast is 0 and the cadence is a fixed
// Slow.
func FromConfig(s config.Sweep) Settings {
	out := Settings{Slow: s.Interval}
	if f := s.Fast; f != nil {
		out.Fast, out.TriggerCPerMin, out.Hold, out.Decay = f.Interval, f.TriggerCPerMin, f.Hold, f.Decay
	}
	return out
}

// Enabled reports whether there is a fast cadence at all.
func (s Settings) Enabled() bool { return s.Fast > 0 }

// at is rule 3, the whole curve, as a pure function of the time since the
// last trigger:
//
//	since < hold:                 Fast
//	hold ≤ since < hold + decay:  Fast × (Slow/Fast) ^ ((since − hold) / decay)
//	since ≥ hold + decay:         Slow
//
// The ease is exponential, so each step up is the same fraction of the
// interval: it starts at exactly Fast, is the geometric mean √(Fast×Slow)
// half way, and reaches Slow at the end. The result is rounded to the
// millisecond (nothing finer means anything for a multi-second sweep) and
// clamped to [Fast, Slow] so float error can never step outside.
//
// atFast is the rule for "still at the fast interval", stated rather than
// read off a rounded interval: since ≤ hold. That's the hold itself, and
// the decay's very first instant (since = hold, where the curve is exactly
// Fast; with hold: 0s, the triggering sweep). A trigger while atFast
// extends the fast spell; one after it is a new fast entry. Just past the
// hold the interval still rounds to exactly Fast, but the cadence has left
// it (TestAtFastIsExplicit).
//
// It's only called with fast enabled; Observe handles "never triggered".
func (s Settings) at(since time.Duration) (mode Mode, interval time.Duration, atFast bool) {
	atFast = since <= s.Hold
	switch {
	case since < s.Hold:
		return Hold, s.Fast, atFast
	case since >= s.Hold+s.Decay:
		return Slow, s.Slow, atFast
	}
	x := float64(since-s.Hold) / float64(s.Decay) // 0 ≤ x < 1
	d := time.Duration(float64(s.Fast) * math.Pow(float64(s.Slow)/float64(s.Fast), x)).Round(time.Millisecond)
	return Decay, min(max(d, s.Fast), s.Slow), atFast
}

// Trigger is a probe changing fast enough to go fast: the fastest such
// probe in one sweep.
type Trigger struct {
	At       time.Time // the sweep that saw the change: its timestamp, Sweep.At (wall clock, as published)
	DeviceID string
	Position string
	ROM      string
	CPerMin  float64 // signed: negative is falling (an inlet during a draw)
	DeltaC   float64 // the raw change since the sweep before, signed
}

// Probe is the probe's identity as statehouse knows it,
// "<device_id>.<position>" (docs/STATEHOUSE.md, Identity), which is how
// /api/status names it.
func (t Trigger) Probe() string { return t.DeviceID + "." + t.Position }

// Decision is what Observe decided after one sweep: the mode and the
// interval until the next sweep is due.
type Decision struct {
	Mode     Mode
	Interval time.Duration
	Trigger  *Trigger // non-nil when this sweep triggered
	// Changed: Mode differs from the decision before. A trigger lands at
	// Fast: in Hold, or, with hold: 0s, at the very start of Decay.
	Changed bool
	// AtFast: still at the fast interval (Settings.at). Decides whether
	// the next trigger is a new fast entry.
	AtFast bool
}

// Line is the verbose log's line for this decision (log.verbose, issue #2),
// or "" when there's nothing to say. Every trigger gets one, and so does
// every other change of mode:
//
//	cadence: fast 10s (hot-water-cylinder/bottom -3.2 °C/min)
//	cadence: decay 10.4s
//	cadence: slow 30s
//
// They start "cadence:", not "sweep", so `grep '^sweep '` still gives the
// readings and `grep '^sweep:'` the faults (GUESSES G42). The probe is
// tank/position, as the journal's other lines name it. The interval is the
// gap to the next sweep, to 0.1 s.
func (d Decision) Line() string {
	iv := d.Interval.Round(100 * time.Millisecond)
	switch {
	case d.Trigger != nil:
		return fmt.Sprintf("cadence: fast %s (%s/%s %+.1f °C/min)", iv, d.Trigger.DeviceID, d.Trigger.Position, d.Trigger.CPerMin)
	case d.Changed:
		return fmt.Sprintf("cadence: %s %s", d.Mode, iv)
	}
	return ""
}

// Status is the cadence's state for /api/status: the decision governing
// the wait for the next sweep, the last trigger and the fast entries.
type Status struct {
	Enabled     bool
	Mode        Mode
	Interval    time.Duration
	LastTrigger *Trigger // nil until the first trigger; a copy
	// FastEntries counts the times since start that a trigger came while
	// the cadence wasn't AtFast (from Slow, or a snap back from the
	// decay). A trigger while AtFast extends the fast spell and isn't
	// counted, so this is roughly "events", not "fast sweeps".
	FastEntries int
}

// last is a probe's usable reading from the previous sweep.
type last struct {
	rawC float64
}

// Cadence is the state between sweeps. It isn't safe for concurrent use:
// run's Work calls Observe, one sweep at a time, and hands Status to the
// status tracker, which is.
type Cadence struct {
	s        Settings
	prevAt   time.Time       // the previous sweep's start, monotonic
	prev     map[string]last // by ROM: probes OK and not suspect in the previous sweep
	anchor   time.Time       // the last trigger's sweep start, monotonic: the curve's t = 0
	trigger  *Trigger        // the last trigger, as observed and reported; nil before the first
	decision Decision        // the last decision
	entries  int
}

// New returns a Cadence that hasn't seen a sweep: slow, at Settings.Slow.
func New(s Settings) *Cadence {
	return &Cadence{s: s, decision: Decision{Mode: Slow, Interval: s.Slow}}
}

// Observe takes the sweep just completed and when it started on the
// monotonic clock (lifecycle hands each unit of work its start time; in
// production that's time.Now, which carries a monotonic reading), and
// decides the interval until the next one. Each sweep is compared only with
// the one observed before it (rule 2), so a sweep must be observed exactly
// once, in order.
func (c *Cadence) Observe(sw sweep.Sweep, started time.Time) Decision {
	trig := c.fastest(sw, started)
	c.remember(sw, started)
	if !c.s.Enabled() {
		c.decision = Decision{Mode: Slow, Interval: c.s.Slow}
		return c.decision
	}

	prev := c.decision
	if trig != nil {
		// An entry is a trigger when the cadence wasn't AtFast: from Slow,
		// or a snap back from the decay. A trigger while AtFast (in the
		// hold, or at the decay's first instant) only extends the spell.
		if !prev.AtFast {
			c.entries++
		}
		c.trigger, c.anchor = trig, started
	}
	d := Decision{Mode: Slow, Interval: c.s.Slow, Trigger: trig}
	if c.trigger != nil {
		// Monotonic, so never negative; max is only for a caller that
		// hands in times out of order.
		d.Mode, d.Interval, d.AtFast = c.s.at(max(0, started.Sub(c.anchor)))
	}
	d.Changed = d.Mode != prev.Mode
	c.decision = d
	return d
}

// fastest is rules 1 and 2: the probe changing fastest since the previous
// sweep, if any is changing at least as fast as the trigger and by at least
// MinChangeC. Ties go to the first in config order. It needs fast on, and a
// positive monotonic gap since the previous sweep: no rate can be had from
// two sweeps at the same instant.
func (c *Cadence) fastest(sw sweep.Sweep, started time.Time) *Trigger {
	if !c.s.Enabled() || c.prev == nil {
		return nil
	}
	dt := started.Sub(c.prevAt)
	if dt <= 0 {
		return nil
	}
	var best *Trigger
	for _, t := range sw.Tanks {
		for _, x := range t.Sensors {
			raw, ok := usable(x)
			if !ok {
				continue
			}
			before, seen := c.prev[x.ROM]
			if !seen {
				continue // first seen, or failed / suspect in the sweep before
			}
			delta := raw - before.rawC
			rate := delta / dt.Minutes()
			if math.Abs(delta) < MinChangeC || math.Abs(rate) < c.s.TriggerCPerMin {
				continue
			}
			if best == nil || math.Abs(rate) > math.Abs(best.CPerMin) {
				best = &Trigger{At: sw.At, DeviceID: t.DeviceID, Position: x.Position, ROM: x.ROM, CPerMin: rate, DeltaC: delta}
			}
		}
	}
	return best
}

// remember keeps this sweep's usable readings, and only those, for the
// next comparison. A probe that isn't usable now has no previous reading
// next time, which is what makes a just-recovered probe unable to trigger.
func (c *Cadence) remember(sw sweep.Sweep, started time.Time) {
	c.prevAt = started
	c.prev = map[string]last{}
	for _, t := range sw.Tanks {
		for _, x := range t.Sensors {
			if raw, ok := usable(x); ok {
				c.prev[x.ROM] = last{rawC: raw}
			}
		}
	}
}

// usable is rule 2 for one reading: OK, with a value, and not a suspect
// 85 °C (which might be a sensor that restarted, not a real temperature).
// It's the raw value, not the calibrated one: the offset is the same on
// both sides of a difference, and raw values are exact sixteenths.
func usable(x sweep.Sensor) (float64, bool) {
	if x.Code != reading.OK || x.RawC == nil || x.Suspect != "" {
		return 0, false
	}
	return *x.RawC, true
}

// Status returns the cadence's state, as a copy.
func (c *Cadence) Status() Status {
	st := Status{
		Enabled:     c.s.Enabled(),
		Mode:        c.decision.Mode,
		Interval:    c.decision.Interval,
		FastEntries: c.entries,
	}
	if c.trigger != nil {
		t := *c.trigger
		st.LastTrigger = &t
	}
	return st
}
