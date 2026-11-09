package cadence

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// The issue's numbers: 30 s slow, 10 s fast, 1.5 °C/min, hold 2m, decay 5m.
var issue = Settings{
	Slow: 30 * time.Second, Fast: 10 * time.Second,
	TriggerCPerMin: 1.5, Hold: 2 * time.Minute, Decay: 5 * time.Minute,
}

var t0 = time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)

// Probes, by ROM. inlet and outlet are on one tank, other on a second, so
// tests can show the trigger is checked on every probe of every tank.
const (
	inlet  = "28-000000000001"
	outlet = "28-000000000002"
	other  = "28-000000000003"
)

// probe is one sensor's outcome in a test sweep: a raw °C value (nil for a
// failed read), and optionally a suspect flag or a failure code.
type probe struct {
	rom     string
	c       *float64
	code    reading.Code
	suspect string
}

func ok(rom string, c float64) probe { return probe{rom: rom, c: &c, code: reading.OK} }
func failed(rom string, code reading.Code) probe {
	return probe{rom: rom, code: code}
}
func suspect(rom string, c float64) probe {
	p := ok(rom, c)
	p.suspect = reading.Suspect85
	return p
}

// sweepAt builds a sweep at t0+at. The first two probes go on tank "cyl",
// any more on tank "second", as the real sweep orders them.
func sweepAt(at time.Duration, ps ...probe) sweep.Sweep {
	sw := sweep.Sweep{At: t0.Add(at), ClockSynced: true}
	positions := map[string]string{inlet: "bottom", outlet: "top", other: "middle"}
	tanks := map[string]int{}
	for _, p := range ps {
		dev := "cyl"
		if p.rom == other {
			dev = "second"
		}
		i, seen := tanks[dev]
		if !seen {
			i = len(sw.Tanks)
			tanks[dev] = i
			sw.Tanks = append(sw.Tanks, sweep.Tank{DeviceID: dev})
		}
		s := sweep.Sensor{ROM: p.rom, Position: positions[p.rom], Code: p.code, Suspect: p.suspect, Attempts: 1}
		if p.c != nil {
			raw, temp := *p.c, *p.c-0.19 // an offset, so a test would notice which value is used
			s.RawC, s.TempC = &raw, &temp
		}
		sw.Tanks[i].Sensors = append(sw.Tanks[i].Sensors, s)
	}
	return sw
}

// steady is a quiet tank: both probes as they were.
func steady(at time.Duration) sweep.Sweep { return sweepAt(at, ok(inlet, 12), ok(outlet, 60)) }

// --- The interval as a function of time since the last trigger (rule 3) ---

func TestIntervalCurve(t *testing.T) {
	// The formula, written out independently of the code under test.
	formula := func(since time.Duration) time.Duration {
		x := float64(since-issue.Hold) / float64(issue.Decay)
		return time.Duration(float64(issue.Fast) * math.Pow(3, x)).Round(time.Millisecond)
	}
	tests := []struct {
		since time.Duration
		mode  Mode
		want  time.Duration
	}{
		{0, Hold, 10 * time.Second},           // the sweep that saw the change
		{time.Minute, Hold, 10 * time.Second}, // mid-hold
		{2*time.Minute - time.Millisecond, Hold, 10 * time.Second},
		{2 * time.Minute, Decay, 10 * time.Second},                                         // the decay starts at fast...
		{2*time.Minute + 150*time.Second, Decay, formula(2*time.Minute + 150*time.Second)}, // ...half way: 10 × √3
		{7*time.Minute - time.Millisecond, Decay, formula(7*time.Minute - time.Millisecond)},
		{7 * time.Minute, Slow, 30 * time.Second}, // ...and ends at slow
		{time.Hour, Slow, 30 * time.Second},
	}
	for _, tt := range tests {
		mode, got := issue.at(tt.since)
		if mode != tt.mode || got != tt.want {
			t.Errorf("%v after a trigger: %s %v, want %s %v", tt.since, mode, got, tt.mode, tt.want)
		}
	}
	// Half way through the decay is the geometric mean of fast and slow.
	if _, mid := issue.at(2*time.Minute + 150*time.Second); mid != 17321*time.Millisecond {
		t.Errorf("mid-decay = %v, want 17.321s (10 s × √3)", mid)
	}
}

// Through the decay the interval only ever rises, and stays within
// [fast, slow], whatever the step.
func TestDecayRisesMonotonicallyWithinBounds(t *testing.T) {
	prev := time.Duration(0)
	for since := issue.Hold; since <= issue.Hold+issue.Decay+time.Second; since += 250 * time.Millisecond {
		_, got := issue.at(since)
		if got < prev {
			t.Fatalf("at %v the interval fell, %v → %v", since, prev, got)
		}
		if got < issue.Fast || got > issue.Slow {
			t.Fatalf("at %v the interval %v is outside [%v, %v]", since, got, issue.Fast, issue.Slow)
		}
		prev = got
	}
}

// --- Observing sweeps (rules 1 and 2) ---

func TestSteadyReadingsStaySlow(t *testing.T) {
	c := New(issue)
	for i := 0; i < 10; i++ {
		d := c.Observe(steady(time.Duration(i) * 30 * time.Second))
		if d.Mode != Slow || d.Interval != 30*time.Second || d.Trigger != nil {
			t.Fatalf("sweep %d: %+v, want slow at 30s and no trigger", i, d)
		}
	}
}

// A 2 °C fall over 30 s is 4 °C/min: fast on the very next tick.
func TestADrawGoesFastAtOnce(t *testing.T) {
	c := New(issue)
	c.Observe(steady(0))
	d := c.Observe(sweepAt(30*time.Second, ok(inlet, 10), ok(outlet, 60)))
	if d.Mode != Hold || d.Interval != 10*time.Second {
		t.Fatalf("after a 2 °C drop: %s %v, want hold at 10s", d.Mode, d.Interval)
	}
	want := Trigger{At: t0.Add(30 * time.Second), DeviceID: "cyl", Position: "bottom", ROM: inlet, CPerMin: -4, DeltaC: -2}
	if d.Trigger == nil || *d.Trigger != want {
		t.Fatalf("trigger = %+v, want %+v", d.Trigger, want)
	}
	if d.Trigger.Probe() != "cyl.bottom" {
		t.Errorf("Probe() = %q, want the statehouse primary cyl.bottom", d.Trigger.Probe())
	}
}

// Rule 1: either direction. A rising outlet triggers as a falling inlet does.
func TestARiseTriggersToo(t *testing.T) {
	c := New(issue)
	c.Observe(steady(0))
	d := c.Observe(sweepAt(30*time.Second, ok(inlet, 12), ok(outlet, 61)))
	if d.Trigger == nil || d.Trigger.ROM != outlet || d.Trigger.CPerMin != 2 {
		t.Fatalf("a 1 °C rise in 30 s: trigger %+v, want outlet at +2 °C/min", d.Trigger)
	}
}

// The rate is |ΔT| / Δt, so the same rate means the same thing at either
// interval: 0.75 °C over 30 s and 0.25 °C over 10 s are both 1.5 °C/min,
// exactly the trigger ("at least this fast"), and both trigger. Just under
// it doesn't.
func TestTheTriggerIsARate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		gap     time.Duration
		deltaC  float64
		trigger bool
	}{
		{"0.75 °C in 30 s, exactly 1.5", 30 * time.Second, 0.75, true},
		{"0.25 °C in 10 s, exactly 1.5", 10 * time.Second, 0.25, true},
		{"0.6875 °C in 30 s, 1.375", 30 * time.Second, 0.6875, false},
		{"0.1875 °C in 10 s, 1.125", 10 * time.Second, 0.1875, false},
	} {
		c := New(issue)
		c.Observe(steady(0))
		d := c.Observe(sweepAt(tt.gap, ok(inlet, 12-tt.deltaC), ok(outlet, 60)))
		if (d.Trigger != nil) != tt.trigger {
			t.Errorf("%s: trigger %+v, want triggered=%v", tt.name, d.Trigger, tt.trigger)
		}
	}
}

// The rate uses the raw value, not the calibrated one: the offset is the
// same on both readings, so it can't change a difference, and raw values
// are exact sixteenths, so the two-step minimum is an exact comparison.
func TestTheRateUsesRawValues(t *testing.T) {
	c := New(issue)
	c.Observe(steady(0))
	d := c.Observe(sweepAt(30*time.Second, ok(inlet, 10), ok(outlet, 60)))
	if d.Trigger == nil || d.Trigger.DeltaC != -2 {
		t.Fatalf("trigger %+v, want ΔT exactly -2 (raw 12 → 10)", d.Trigger)
	}
}

// One DS18B20 step (0.0625 °C) between 2 s sweeps is 1.875 °C/min, over
// the trigger. It still never triggers: a change must be at least two steps.
func TestAOneStepFlickerNeverTriggers(t *testing.T) {
	s := issue
	s.Fast = 2 * time.Second
	c := New(s)
	v := 12.0
	for i := 0; i < 20; i++ {
		if i%2 == 1 {
			v = 12.0625
		} else {
			v = 12.0
		}
		d := c.Observe(sweepAt(time.Duration(i)*2*time.Second, ok(inlet, v), ok(outlet, 60)))
		if d.Trigger != nil {
			t.Fatalf("sweep %d: a one-step flicker triggered: %+v", i, d.Trigger)
		}
	}
	// Two steps in 2 s (3.75 °C/min) does.
	d := c.Observe(sweepAt(40*time.Second, ok(inlet, v+0.125), ok(outlet, 60)))
	if d.Trigger == nil {
		t.Errorf("two steps in 2 s didn't trigger")
	}
}

// Rule 2: only OK readings, with an OK reading in the sweep before, count.
func TestOnlyTwoOKReadingsCanTrigger(t *testing.T) {
	tests := []struct {
		name       string
		prev, curr probe
	}{
		{"failed now", ok(inlet, 12), failed(inlet, reading.CRC)},
		{"failed before (just recovered)", failed(inlet, reading.BusLow), ok(inlet, 0)},
		{"not present before", failed(inlet, reading.NotPresent), ok(inlet, 40)},
		{"OK without a value (a broken Source)", ok(inlet, 12), probe{rom: inlet, code: reading.OK}},
		{"an 85 °C suspect now", ok(inlet, 60), suspect(inlet, 85)},
		{"an 85 °C suspect before", suspect(inlet, 85), ok(inlet, 60)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(issue)
			c.Observe(sweepAt(0, tt.prev, ok(outlet, 60)))
			if d := c.Observe(sweepAt(30*time.Second, tt.curr, ok(outlet, 60))); d.Trigger != nil {
				t.Errorf("triggered: %+v", d.Trigger)
			}
		})
	}
}

// A probe seen for the first time (the first sweep, or one that wasn't in
// the sweep before) has nothing to compare with.
func TestFirstSeenNeverTriggers(t *testing.T) {
	c := New(issue)
	if d := c.Observe(sweepAt(0, ok(inlet, 85), ok(outlet, 10))); d.Trigger != nil || d.Mode != Slow {
		t.Fatalf("first sweep: %+v", d)
	}
	if d := c.Observe(sweepAt(30*time.Second, ok(inlet, 85), ok(outlet, 10), ok(other, 70))); d.Trigger != nil {
		t.Fatalf("a probe new to this sweep triggered: %+v", d.Trigger)
	}
}

// The comparison is always with the sweep just before: a probe that was
// OK, then failed, then is OK again has no previous OK reading, even
// though there's an older one.
func TestRecoveredComparesWithTheSweepJustBefore(t *testing.T) {
	c := New(issue)
	c.Observe(sweepAt(0, ok(inlet, 12), ok(outlet, 60)))
	c.Observe(sweepAt(30*time.Second, failed(inlet, reading.CRC), ok(outlet, 60)))
	if d := c.Observe(sweepAt(60*time.Second, ok(inlet, 5), ok(outlet, 60))); d.Trigger != nil {
		t.Errorf("recovered probe triggered against an older reading: %+v", d.Trigger)
	}
}

// Every probe of every tank is checked, and the fastest one is reported.
func TestTheFastestProbeOnAnyTankIsTheTrigger(t *testing.T) {
	c := New(issue)
	c.Observe(sweepAt(0, ok(inlet, 12), ok(outlet, 60), ok(other, 40)))
	d := c.Observe(sweepAt(30*time.Second, ok(inlet, 11), ok(outlet, 61.5), ok(other, 36)))
	if d.Trigger == nil || d.Trigger.DeviceID != "second" || d.Trigger.Position != "middle" || d.Trigger.CPerMin != -8 {
		t.Fatalf("trigger = %+v, want second/middle at -8 °C/min (the fastest of -2, +3, -8)", d.Trigger)
	}
}

// Two sweeps at the same instant, or a clock that stepped back between
// them, give no rate at all, rather than an infinite or negative one.
func TestNoRateWithoutTimeBetween(t *testing.T) {
	for _, gap := range []time.Duration{0, -10 * time.Second} {
		c := New(issue)
		c.Observe(steady(time.Minute))
		if d := c.Observe(sweepAt(time.Minute+gap, ok(inlet, 0), ok(outlet, 60))); d.Trigger != nil {
			t.Errorf("gap %v: triggered %+v", gap, d.Trigger)
		}
	}
}

// --- Hold, decay, and snapping back (rule 3) ---

// drive feeds the cadence one sweep per interval it asks for, starting with
// a trigger at t0, then steady readings, and returns each sweep's offset
// from t0 and its decision.
func drive(t *testing.T, s Settings, until time.Duration) ([]time.Duration, []Decision) {
	t.Helper()
	c := New(s)
	c.Observe(steady(-30 * time.Second))
	var at time.Duration
	var times []time.Duration
	var ds []Decision
	d := c.Observe(sweepAt(0, ok(inlet, 2), ok(outlet, 60))) // a 10 °C fall: 20 °C/min
	if d.Trigger == nil {
		t.Fatal("the opening draw didn't trigger")
	}
	for at < until {
		times, ds = append(times, at), append(ds, d)
		at += d.Interval
		d = c.Observe(sweepAt(at, ok(inlet, 2), ok(outlet, 60)))
	}
	return times, ds
}

// The issue's worked example: with 10 s / 30 s, hold 2m and decay 5m, a
// single event gets 12 sweeps at 10 s (and the 13th, at the end of the
// hold, is still 10 s away from the next), then eases out to 30 s: 33
// sweeps in the 8 minutes after it, where a fixed 30 s would give 16.
func TestTheIssuesWorkedExample(t *testing.T) {
	times, ds := drive(t, issue, 8*time.Minute)
	if len(times) != 33 {
		t.Errorf("%d sweeps in the 8 minutes after a trigger, want 33: %v", len(times), times)
	}
	holds := 0
	for i, d := range ds {
		if d.Mode == Hold {
			holds++
			if d.Interval != issue.Fast {
				t.Errorf("sweep %d in hold at %v", i, d.Interval)
			}
		}
	}
	if holds != 12 {
		t.Errorf("%d sweeps in hold, want 12 (0 s to 110 s)", holds)
	}
	// Then the decay: rising every sweep, until slow.
	for i := 13; i < len(ds); i++ {
		if ds[i].Interval <= ds[i-1].Interval && ds[i].Mode == Decay {
			t.Errorf("sweep %d: decay interval %v didn't rise from %v", i, ds[i].Interval, ds[i-1].Interval)
		}
	}
	// Steady again: back to slow, and it stays there.
	_, ds = drive(t, issue, 15*time.Minute)
	if last := ds[len(ds)-1]; last.Mode != Slow || last.Interval != issue.Slow {
		t.Errorf("15 minutes after one trigger: %s %v, want slow at 30s", last.Mode, last.Interval)
	}
}

// A trigger part-way through the decay snaps straight back to fast.interval
// and restarts the hold.
func TestATriggerInTheDecaySnapsBackToFast(t *testing.T) {
	c := New(issue)
	c.Observe(steady(-30 * time.Second))
	c.Observe(sweepAt(0, ok(inlet, 2), ok(outlet, 60))) // trigger at 0
	c.Observe(sweepAt(4*time.Minute, ok(inlet, 2), ok(outlet, 60)))
	d := c.Observe(sweepAt(4*time.Minute+20*time.Second, ok(inlet, 2), ok(outlet, 60)))
	if d.Mode != Decay || d.Interval <= issue.Fast {
		t.Fatalf("4m20s after the trigger: %s %v, want decaying above 10s", d.Mode, d.Interval)
	}
	// The second draw.
	d = c.Observe(sweepAt(4*time.Minute+40*time.Second, ok(inlet, 0), ok(outlet, 60)))
	if d.Mode != Hold || d.Interval != issue.Fast || d.Trigger == nil {
		t.Fatalf("a trigger mid-decay: %s %v %+v, want hold at 10s", d.Mode, d.Interval, d.Trigger)
	}
	// The hold runs from the new trigger, not the old one.
	d = c.Observe(sweepAt(6*time.Minute+30*time.Second, ok(inlet, 0), ok(outlet, 60)))
	if d.Mode != Hold {
		t.Errorf("1m50s after the second trigger: %s, want still hold", d.Mode)
	}
}

// hold: 0s goes straight into the decay: the triggering sweep is already
// at t = hold, so it's Decay, at exactly Fast (the curve's start).
// Repeated triggers there are one fast entry, not one each: the cadence
// never left fast.
func TestZeroHold(t *testing.T) {
	s := issue
	s.Hold = 0
	_, ds := drive(t, s, time.Minute)
	if ds[0].Mode != Decay || ds[0].Interval != s.Fast {
		t.Errorf("the triggering sweep: %s %v, want decay at fast", ds[0].Mode, ds[0].Interval)
	}
	if ds[1].Mode != Decay || ds[1].Interval <= s.Fast {
		t.Errorf("one sweep later: %s %v, want decaying above fast", ds[1].Mode, ds[1].Interval)
	}

	c := New(s)
	at := time.Duration(0)
	for _, v := range []float64{12, 10, 8, 6} { // three triggers in a row
		at += c.Observe(sweepAt(at, ok(inlet, v), ok(outlet, 60))).Interval
	}
	if st := c.Status(); st.FastEntries != 1 {
		t.Errorf("hold 0, three triggers in a row: FastEntries = %d, want 1", st.FastEntries)
	}
}

// A clock that steps back past the last trigger (an NTP correction) must
// not leave the cadence fast until the clock catches up. The trigger is
// re-anchored at the sweep, so the fast spell is at most hold + decay.
func TestAClockStepBackIsBounded(t *testing.T) {
	c := New(issue)
	c.Observe(steady(-30 * time.Second))
	c.Observe(sweepAt(0, ok(inlet, 2), ok(outlet, 60))) // trigger at t0
	d := c.Observe(sweepAt(-time.Hour, ok(inlet, 2), ok(outlet, 60)))
	if d.Mode != Hold {
		t.Fatalf("an hour before the trigger: %s, want hold (re-anchored)", d.Mode)
	}
	d = c.Observe(sweepAt(-time.Hour+8*time.Minute, ok(inlet, 2), ok(outlet, 60)))
	if d.Mode != Slow {
		t.Errorf("8 minutes after the step back: %s %v, want slow", d.Mode, d.Interval)
	}
	if st := c.Status(); st.LastTrigger == nil || !st.LastTrigger.At.Equal(t0.Add(-time.Hour)) {
		t.Errorf("last trigger after the step back: %+v, want re-anchored to the sweep", st.LastTrigger)
	}
}

// --- With sweep.fast absent ---

func TestWithoutFastItIsAlwaysSlow(t *testing.T) {
	c := New(FromConfig(config.Sweep{Interval: 45 * time.Second}))
	c.Observe(steady(0))
	for i, sw := range []sweep.Sweep{
		sweepAt(45*time.Second, ok(inlet, 0), ok(outlet, 80)), // a huge change
		steady(90 * time.Second),
	} {
		d := c.Observe(sw)
		if d.Mode != Slow || d.Interval != 45*time.Second || d.Trigger != nil {
			t.Errorf("sweep %d: %+v, want slow at 45s", i, d)
		}
	}
	if st := c.Status(); st.Enabled || st.FastEntries != 0 || st.LastTrigger != nil {
		t.Errorf("status = %+v", st)
	}
}

func TestFromConfig(t *testing.T) {
	got := FromConfig(config.Sweep{Interval: 30 * time.Second, Fast: &config.Fast{
		Interval: 10 * time.Second, TriggerCPerMin: 1.5, Hold: 2 * time.Minute, Decay: 5 * time.Minute}})
	if got != issue {
		t.Errorf("FromConfig = %+v, want %+v", got, issue)
	}
	if !got.Enabled() {
		t.Error("with sweep.fast, Enabled() = false")
	}
	if off := FromConfig(config.Sweep{Interval: 30 * time.Second}); off.Enabled() || off.Slow != 30*time.Second {
		t.Errorf("without sweep.fast: %+v", off)
	}
}

// --- Status and fast_entries ---

// FastEntries counts the times the cadence went fast from slow or from the
// decay. A trigger during the hold extends it and isn't a new entry.
func TestFastEntriesCountEntriesNotTriggers(t *testing.T) {
	c := New(issue)
	if st := c.Status(); st.Mode != Slow || st.Interval != issue.Slow || st.LastTrigger != nil || st.FastEntries != 0 || !st.Enabled {
		t.Fatalf("before any sweep: %+v", st)
	}
	temps := []float64{12, 10, 8, 6, 6, 6} // a draw over four sweeps, then quiet
	at := time.Duration(0)
	for _, v := range temps {
		d := c.Observe(sweepAt(at, ok(inlet, v), ok(outlet, 60)))
		at += d.Interval
	}
	if st := c.Status(); st.FastEntries != 1 {
		t.Errorf("one draw, three triggers in a row: FastEntries = %d, want 1", st.FastEntries)
	}
	// A second draw after it's slow again is a second entry.
	at += 10 * time.Minute
	c.Observe(sweepAt(at, ok(inlet, 6), ok(outlet, 60)))
	d := c.Observe(sweepAt(at+30*time.Second, ok(inlet, 3), ok(outlet, 60)))
	st := c.Status()
	if st.FastEntries != 2 || st.Mode != Hold || st.Interval != issue.Fast {
		t.Errorf("second draw: %+v", st)
	}
	if st.LastTrigger == nil || *st.LastTrigger != *d.Trigger {
		t.Errorf("LastTrigger = %+v, want %+v", st.LastTrigger, d.Trigger)
	}
	// Status is a copy.
	st.LastTrigger.CPerMin = 99
	if c.Status().LastTrigger.CPerMin == 99 {
		t.Error("Status shares its LastTrigger with the cadence")
	}
}

// --- The verbose log lines ---

func TestLogLines(t *testing.T) {
	c := New(issue)
	var lines []string
	log := func(d Decision) {
		if l := d.Line(); l != "" {
			lines = append(lines, l)
		}
	}
	log(c.Observe(steady(0)))                                                // slow, as it was: nothing
	log(c.Observe(sweepAt(30*time.Second, ok(inlet, 10), ok(outlet, 60))))   // trigger
	log(c.Observe(sweepAt(40*time.Second, ok(inlet, 9.5), ok(outlet, 60))))  // trigger again, in hold
	log(c.Observe(sweepAt(50*time.Second, ok(inlet, 9.5), ok(outlet, 60))))  // hold, quiet: nothing
	log(c.Observe(sweepAt(170*time.Second, ok(inlet, 9.5), ok(outlet, 60)))) // decaying: 10 s after the hold
	log(c.Observe(sweepAt(200*time.Second, ok(inlet, 9.5), ok(outlet, 60)))) // still decaying: nothing
	log(c.Observe(sweepAt(10*time.Minute, ok(inlet, 9.5), ok(outlet, 60))))  // slow again
	want := []string{
		"cadence: fast 10s (cyl/bottom -4.0 °C/min)",
		"cadence: fast 10s (cyl/bottom -3.0 °C/min)",
		"cadence: decay 10.4s",
		"cadence: slow 30s",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// With fast off, nothing is ever logged.
	off := New(Settings{Slow: 30 * time.Second})
	off.Observe(steady(0))
	if l := off.Observe(sweepAt(30*time.Second, ok(inlet, 0), ok(outlet, 60))).Line(); l != "" {
		t.Errorf("fast off logged %q", l)
	}
}
