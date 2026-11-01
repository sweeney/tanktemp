// Package status holds what tanktemp knows about itself (the startup report,
// the last sweep, MQTT state) behind a lock, and decides its health from it.
// /healthz, the status page and the MQTT status message all read the same
// Snapshot, so they can never disagree.
package status

import (
	"fmt"
	"sync"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/startup"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// Health statuses, as /healthz reports them.
const (
	StatusOK       = "ok"
	StatusStarting = "starting"
	StatusDegraded = "degraded"
)

// StaleAfter is how many sweep intervals may pass before the last sweep
// counts as stale.
const StaleAfter = 3

// Options configure a Tracker.
type Options struct {
	Start     time.Time
	Version   string
	Tanks     []string // device IDs, in config order
	Interval  time.Duration
	Connected func() bool // whether MQTT is connected right now
}

// Tracker is safe for concurrent use.
type Tracker struct {
	opts Options

	mu       sync.Mutex
	problems []startup.Problem
	last     *sweep.Sweep
	sweeps   int
	skipped  int
}

// New returns a Tracker.
func New(o Options) *Tracker {
	if o.Connected == nil {
		o.Connected = func() bool { return false }
	}
	return &Tracker{opts: o}
}

// SetStartup records the startup checks.
func (t *Tracker) SetStartup(r startup.Report) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.problems = append([]startup.Problem(nil), r.Problems...)
}

// RecordSweep records a completed sweep.
func (t *Tracker) RecordSweep(s sweep.Sweep) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last = &s
	t.sweeps++
}

// SkippedTick counts a tick that arrived while a sweep was still running.
func (t *Tracker) SkippedTick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.skipped++
}

// Snapshot is a consistent copy of the tracker's state.
type Snapshot struct {
	Start         time.Time
	Version       string
	Tanks         []string
	Interval      time.Duration
	MQTTConnected bool
	Ready         bool
	Problems      []startup.Problem
	Last          *sweep.Sweep // nil before the first sweep
	Sweeps        int
	SkippedTicks  int
}

// Snapshot returns a copy.
func (t *Tracker) Snapshot() Snapshot {
	connected := t.opts.Connected()
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Snapshot{
		Start:         t.opts.Start,
		Version:       t.opts.Version,
		Tanks:         append([]string(nil), t.opts.Tanks...),
		Interval:      t.opts.Interval,
		MQTTConnected: connected,
		Ready:         t.last != nil,
		Problems:      append([]startup.Problem(nil), t.problems...),
		Sweeps:        t.sweeps,
		SkippedTicks:  t.skipped,
	}
	if t.last != nil {
		l := *t.last // sweeps are never mutated after recording
		s.Last = &l
	}
	return s
}

// Health is the aggregated state: a status and one line per cause.
type Health struct {
	Status string
	Detail []string
}

// Health decides the status at now. starting: no sweep yet. degraded: MQTT
// down, any sensor failing, the last sweep stale, the clock not synced, a
// failed discovery, any startup problem, or a suspect 85 °C value (which is
// still published: it's probably real, but someone should look).
func (s Snapshot) Health(now time.Time) Health {
	var h Health
	if !s.MQTTConnected {
		h.Detail = append(h.Detail, "MQTT disconnected: sweeps are not being delivered")
	}
	for _, p := range s.Problems {
		h.Detail = append(h.Detail, "startup: "+p.Message)
	}
	if s.Last == nil {
		h.Status = StatusStarting
		h.Detail = append([]string{"no complete sweep yet"}, h.Detail...)
		return h
	}
	if age := now.Sub(s.Last.At); age > StaleAfter*s.Interval {
		h.Detail = append(h.Detail, fmt.Sprintf("last sweep %s ago (interval %s)", age.Truncate(time.Second), s.Interval))
	}
	if !s.Last.ClockSynced {
		h.Detail = append(h.Detail, "clock not NTP-synced: timestamps may be wrong")
	}
	if s.Last.DiscoverError != "" {
		h.Detail = append(h.Detail, "discovery failed: "+s.Last.DiscoverError)
	}
	for _, t := range s.Last.Tanks {
		for _, x := range t.Sensors {
			if x.Code != reading.OK {
				h.Detail = append(h.Detail, fmt.Sprintf("%s %s (%s): %s", t.DeviceID, x.Position, x.ROM, x.Code))
			} else if x.Suspect != "" {
				h.Detail = append(h.Detail, fmt.Sprintf("%s %s (%s): published but %s", t.DeviceID, x.Position, x.ROM, x.Suspect))
			}
		}
	}
	h.Status = StatusOK
	if len(h.Detail) > 0 {
		h.Status = StatusDegraded
	}
	return h
}
