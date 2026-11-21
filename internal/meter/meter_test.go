package meter

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const ms = time.Millisecond

var cfg = Config{Hold: 20 * ms, MinInterval: 600 * ms}

// feed runs edges through a fresh counter (seqnos 1, 2, 3, ...), then lets
// the last edge's hold pass (a Tick at its time + Hold), and returns the
// counter with every event it emitted, in order.
func feed(t *testing.T, c Config, initial Level, edges ...Edge) (*Counter, []Event) {
	t.Helper()
	k, err := New(c, initial)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for i, e := range edges {
		if e.Seqno == 0 {
			e.Seqno = uint64(i + 1)
		}
		evs = append(evs, k.Observe(e)...)
	}
	if len(edges) > 0 {
		evs = append(evs, k.Tick(edges[len(edges)-1].At+c.Hold)...)
	}
	return k, evs
}

func at(d time.Duration, l Level) Edge { return Edge{At: d, Level: l} }

func kinds(evs []Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, string(e.Kind))
	}
	return strings.Join(s, ",")
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, c := range []Config{
		{Hold: -1, MinInterval: 600 * ms},
		{Hold: 20 * ms, MinInterval: -1},
		{Hold: 600 * ms, MinInterval: 600 * ms}, // the hold must be shorter than the backstop
	} {
		if _, err := New(c, Open); err == nil {
			t.Errorf("New(%+v) accepted", c)
		}
	}
	if _, err := New(cfg, Level(7)); err == nil {
		t.Error("an unknown initial level was accepted")
	}
}

// One clean pulse: open -> closed -> open is one litre. The pulse is
// confirmed once the closing has held for Hold (here by the opening, which
// comes later), stamped at the closing, and the open reports how long the
// reed stayed closed.
func TestOnePulse(t *testing.T) {
	k, evs := feed(t, cfg, Open, at(1000*ms, Closed), at(1400*ms, Open))
	if got := kinds(evs); got != "pulse,open" {
		t.Fatalf("events = %s, want pulse,open", got)
	}
	if evs[0].At != 1000*ms || evs[1].At != 1400*ms {
		t.Errorf("event times = %v, %v; want the closing (1s) and the opening (1.4s)", evs[0].At, evs[1].At)
	}
	s := k.Snapshot()
	if s.Count != 1 || s.Level != Open || s.LastPulseAt == nil || *s.LastPulseAt != 1000*ms ||
		s.LastInterval != nil || s.LastClosed == nil || *s.LastClosed != 400*ms {
		t.Errorf("snapshot = %+v", s)
	}
}

// The second pulse gives the interval between counted closings.
func TestInterval(t *testing.T) {
	k, _ := feed(t, cfg, Open,
		at(1000*ms, Closed), at(1400*ms, Open),
		at(2840*ms, Closed), at(3240*ms, Open))
	s := k.Snapshot()
	if s.Count != 2 || s.LastInterval == nil || *s.LastInterval != 1840*ms || *s.LastPulseAt != 2840*ms {
		t.Errorf("snapshot = %+v", s)
	}
}

// Tick confirms a held closing without waiting for the opening (the pointer
// can rest on the magnet for hours), stamped at the closing, not at the tick.
func TestTickConfirmsHeldClosing(t *testing.T) {
	k, _ := New(cfg, Open)
	evs := k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	if len(evs) != 0 {
		t.Fatalf("a closing was confirmed before its hold: %s", kinds(evs))
	}
	if evs := k.Tick(1019 * ms); len(evs) != 0 {
		t.Fatalf("confirmed 19 ms in, before the 20 ms hold: %s", kinds(evs))
	}
	evs = k.Tick(1020 * ms)
	if kinds(evs) != "pulse" || evs[0].At != 1000*ms {
		t.Fatalf("Tick at the hold = %s at %v, want pulse at 1s", kinds(evs), evs)
	}
	if evs := k.Tick(5000 * ms); len(evs) != 0 {
		t.Errorf("a second Tick confirmed again: %s", kinds(evs))
	}
	evs = k.Observe(Edge{At: 9000 * ms, Level: Open, Seqno: 2})
	evs = append(evs, k.Tick(9020*ms)...)
	if kinds(evs) != "open" || *k.Snapshot().LastClosed != 8000*ms {
		t.Errorf("after the rest: %s, %+v", kinds(evs), k.Snapshot())
	}
	if k.Snapshot().Count != 1 {
		t.Errorf("count = %d, want 1", k.Snapshot().Count)
	}
}

// An edge that reports the level the counter already holds is noise: it
// changes nothing. This is what the night's spikes looked like.
func TestSameLevelIsNoise(t *testing.T) {
	k, evs := feed(t, cfg, Open,
		at(1000*ms, Open), // already open
		at(2000*ms, Closed), at(2400*ms, Open),
		at(2500*ms, Open), // already open again
		at(3000*ms, Closed),
		at(3000*ms+73*time.Microsecond, Closed), // already closed (the 18:15 burst)
		at(3000*ms+85*time.Microsecond, Closed),
		at(3500*ms, Open))
	s := k.Snapshot()
	if s.Count != 2 || s.Noise != 4 || s.Short != 0 || s.Glitches != 0 {
		t.Errorf("snapshot = %+v, want count 2, noise 4", s)
	}
	if got := kinds(evs); got != "pulse,open,pulse,open" {
		t.Errorf("events = %s", got)
	}
}

// A closing that reopens within Hold is a spike that flipped the reading:
// both edges are dropped as one "short", and nothing is counted.
func TestShortClosingIsDropped(t *testing.T) {
	k, evs := feed(t, cfg, Open, at(1000*ms, Closed), at(1005*ms, Open))
	s := k.Snapshot()
	if len(evs) != 0 || s.Count != 0 || s.Short != 1 || s.Level != Open || s.LastClosed != nil {
		t.Errorf("events %s, snapshot %+v", kinds(evs), s)
	}
	// And the counter carries on normally.
	_ = k.Observe(Edge{At: 2000 * ms, Level: Closed, Seqno: 3})
	evs = k.Observe(Edge{At: 2400 * ms, Level: Open, Seqno: 4})
	evs = append(evs, k.Tick(2420*ms)...)
	if kinds(evs) != "pulse,open" || k.Snapshot().Count != 1 {
		t.Errorf("after a short: %s, %+v", kinds(evs), k.Snapshot())
	}
}

// Hold 0 confirms a closing at once.
func TestZeroHold(t *testing.T) {
	_, evs := feed(t, Config{Hold: 0, MinInterval: 600 * ms}, Open, at(1000*ms, Closed))
	if kinds(evs) != "pulse" {
		t.Errorf("events = %s, want pulse at once", kinds(evs))
	}
}

// A closing less than MinInterval after the last COUNTED closing is a
// glitch: not counted, and its opening reports nothing.
func TestMinIntervalGlitch(t *testing.T) {
	k, evs := feed(t, cfg, Open,
		at(1000*ms, Closed), at(1100*ms, Open),
		at(1500*ms, Closed), at(1550*ms, Open), // 500 ms after the counted one
		at(1700*ms, Closed), at(1800*ms, Open)) // 700 ms after the counted one
	s := k.Snapshot()
	if s.Count != 2 || s.Glitches != 1 || *s.LastInterval != 700*ms {
		t.Errorf("snapshot = %+v, want count 2, glitches 1, interval 700ms", s)
	}
	if got := kinds(evs); got != "pulse,open,pulse,open" {
		t.Errorf("events = %s", got)
	}
}

// Order of checks: same level first, then the hold, then the interval. A
// same-level edge during a held closing is noise and keeps the closing.
func TestNoiseDuringHold(t *testing.T) {
	k, evs := feed(t, cfg, Open, at(1000*ms, Closed), at(1001*ms, Closed), at(1400*ms, Open))
	s := k.Snapshot()
	if kinds(evs) != "pulse,open" || s.Noise != 1 || s.Short != 0 || *s.LastPulseAt != 1000*ms {
		t.Errorf("events %s, snapshot %+v", kinds(evs), s)
	}
}

// A seqno gap means the kernel dropped edges: count them as lost, take the
// reported level as truth, and never count a litre on the resync. A held
// closing pending at the gap is dropped too: its fate is unknown.
func TestSeqnoGapResyncs(t *testing.T) {
	k, err := New(cfg, Open)
	if err != nil {
		t.Fatal(err)
	}
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	k.Observe(Edge{At: 1400 * ms, Level: Open, Seqno: 2})
	k.Observe(Edge{At: 3000 * ms, Level: Closed, Seqno: 3}) // held, not yet confirmed
	evs := k.Observe(Edge{At: 3010 * ms, Level: Closed, Seqno: 7})
	s := k.Snapshot()
	if len(evs) != 0 || s.Count != 1 || s.Lost != 3 || s.Level != Closed || s.Noise != 0 {
		t.Errorf("gap: events %s, snapshot %+v; want count 1, lost 3, level closed", kinds(evs), s)
	}
	if evs := k.Tick(9000 * ms); len(evs) != 0 {
		t.Errorf("a resynced closing was confirmed: %s", kinds(evs))
	}
	// Its opening isn't the end of a counted pulse, so it reports nothing.
	if evs := k.Observe(Edge{At: 3500 * ms, Level: Open, Seqno: 8}); len(evs) != 0 {
		t.Errorf("the resynced pulse's opening reported: %s", kinds(evs))
	}
	// The next real pulse counts.
	k.Observe(Edge{At: 5000 * ms, Level: Closed, Seqno: 9})
	if evs := append(k.Observe(Edge{At: 5400 * ms, Level: Open, Seqno: 10}), k.Tick(5420*ms)...); kinds(evs) != "pulse,open" || k.Snapshot().Count != 2 {
		t.Errorf("after the resync: %s, %+v", kinds(evs), k.Snapshot())
	}
}

// A closing still held when a gap arrives is dropped even if the gap edge
// comes after its hold: the dropped edges may have reopened it. (A Tick
// before the gap would have confirmed it; nothing did.)
func TestSeqnoGapDropsHeldClosingPastItsHold(t *testing.T) {
	k, _ := New(cfg, Open)
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	evs := k.Observe(Edge{At: 5000 * ms, Level: Open, Seqno: 4})
	if s := k.Snapshot(); len(evs) != 0 || s.Count != 0 || s.Lost != 2 || s.Level != Open {
		t.Errorf("events %s, snapshot %+v; want nothing counted, lost 2", kinds(evs), s)
	}
}

// A repeated or backwards seqno isn't a gap: the kernel shouldn't send one,
// but if it does (a fake, a re-request bug) the edge is ignored as noise and
// a held closing is kept, so no litre vanishes without trace (review, #1).
func TestRepeatedSeqnoKeepsPending(t *testing.T) {
	k, _ := New(cfg, Open)
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	k.Observe(Edge{At: 1001 * ms, Level: Closed, Seqno: 1}) // repeat
	k.Observe(Edge{At: 1002 * ms, Level: Open, Seqno: 0})   // backwards
	evs := k.Tick(2000 * ms)
	s := k.Snapshot()
	if kinds(evs) != "pulse" || s.Count != 1 || s.Noise != 2 || s.Lost != 0 || s.Level != Closed {
		t.Errorf("events %s, snapshot %+v; want the held closing counted, noise 2, lost 0", kinds(evs), s)
	}
	if evs := append(k.Observe(Edge{At: 2400 * ms, Level: Open, Seqno: 2}), k.Tick(2420*ms)...); kinds(evs) != "open" {
		t.Errorf("the next real edge: %s", kinds(evs))
	}
}

// The kernel numbers a request's events from 1 (review on #9: the first
// edge used to set the base, so a loss before it went uncounted). A first
// edge numbered 41 is a gap of 40, and resyncs like any other.
func TestFirstSeqnoAfterOneIsALoss(t *testing.T) {
	k, _ := feed(t, cfg, Open, Edge{At: 1000 * ms, Level: Closed, Seqno: 41}, Edge{At: 1400 * ms, Level: Open, Seqno: 42})
	if s := k.Snapshot(); s.Lost != 40 || s.Count != 0 {
		t.Errorf("snapshot = %+v, want lost 40 and nothing counted across it", s)
	}
}

// The initial level, read when the line is requested, is the starting state:
// starting closed (the pointer resting on the magnet), the first opening ends
// no counted pulse, and the litre in progress isn't counted.
func TestStartClosed(t *testing.T) {
	k, evs := feed(t, cfg, Closed, at(1000*ms, Open), at(2000*ms, Closed), at(2400*ms, Open))
	if kinds(evs) != "pulse,open" || k.Snapshot().Count != 1 {
		t.Errorf("events %s, snapshot %+v", kinds(evs), k.Snapshot())
	}
}

// readEdges parses a gpiomon recording: "epoch_seconds.ns edge" (1 = rising =
// open, 2 = falling = closed), after # comments. Seqnos are 1, 2, 3...
func readEdges(t *testing.T, path string) []Edge {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Edge
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[1] != "1" && fields[1] != "2") {
			t.Fatalf("bad line %q", line)
		}
		l := Open
		if fields[1] == "2" {
			l = Closed
		}
		out = append(out, Edge{At: stamp(t, fields[0]), Level: l, Seqno: uint64(len(out) + 1)})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// stamp turns "epoch_seconds.ns" into a Duration, exactly.
func stamp(t *testing.T, ts string) time.Duration {
	t.Helper()
	sec, frac, _ := strings.Cut(ts, ".")
	s, err1 := strconv.ParseInt(sec, 10, 64)
	ns, err2 := strconv.ParseInt((frac + "000000000")[:9], 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("bad time %q", ts)
	}
	return time.Duration(s)*time.Second + time.Duration(ns)
}

// expect is a recording's expect.json: what the counter must make of it.
type expect struct {
	Synthetic bool `json:"synthetic"`
	Config    struct {
		HoldMS        int `json:"hold_ms"`
		MinIntervalMS int `json:"min_interval_ms"`
	} `json:"config"`
	InitialLevel string `json:"initial_level"`
	Want         struct {
		Count, Opens, Noise, Short, Glitches, Lost uint64
	} `json:"want"`
	WantDebounced struct {
		Count, Opens, Noise uint64
	} `json:"want_debounced"`
	LastClosedAtOpen    map[string]int64 `json:"last_closed_ns_at_open"`
	LastIntervalAtPulse map[string]int64 `json:"last_interval_ns_at_pulse"`
}

// replay is a recording with its expect.json, run through a counter.
type replay struct {
	exp    expect
	cfg    Config
	level  Level
	edges  []Edge
	k      *Counter
	pulses uint64
	opens  uint64
	// The snapshot's LastClosed at each open, and LastInterval at each
	// pulse, keyed by the edge's time.
	closedAt, intervalAt map[time.Duration]time.Duration
}

func loadReplay(t *testing.T, dir string) *replay {
	t.Helper()
	b, err := os.ReadFile(dir + "/expect.json")
	if err != nil {
		t.Fatal(err)
	}
	r := &replay{}
	if err := json.Unmarshal(b, &r.exp); err != nil {
		t.Fatal(err)
	}
	r.cfg = Config{Hold: time.Duration(r.exp.Config.HoldMS) * ms, MinInterval: time.Duration(r.exp.Config.MinIntervalMS) * ms}
	r.level = map[string]Level{"open": Open, "closed": Closed}[r.exp.InitialLevel]
	r.edges = readEdges(t, dir+"/gpio27.edges")
	return r
}

func (r *replay) run(t *testing.T, edges []Edge) {
	t.Helper()
	k, err := New(r.cfg, r.level)
	if err != nil {
		t.Fatal(err)
	}
	r.k, r.pulses, r.opens = k, 0, 0
	r.closedAt, r.intervalAt = map[time.Duration]time.Duration{}, map[time.Duration]time.Duration{}
	note := func(evs []Event) {
		for _, e := range evs {
			s := k.Snapshot()
			switch e.Kind {
			case Pulse:
				r.pulses++
				if s.LastInterval != nil {
					r.intervalAt[e.At] = *s.LastInterval
				}
			case Opened:
				r.opens++
				r.closedAt[e.At] = *s.LastClosed
			}
		}
	}
	for _, e := range edges {
		note(k.Observe(e))
	}
	note(k.Tick(edges[len(edges)-1].At + time.Hour))
}

const firstNight = "testdata/first-night"

// The first night, raw, with no kernel debounce: the spikes reach the
// counter, and the state machine rejects every one.
func TestReplayFirstNight(t *testing.T) {
	r := loadReplay(t, firstNight)
	if r.exp.Synthetic || len(r.edges) != 277 {
		t.Fatalf("synthetic %v, %d edges; want the real capture's 277", r.exp.Synthetic, len(r.edges))
	}
	r.run(t, r.edges)
	s, w := r.k.Snapshot(), r.exp.Want
	if s.Count != w.Count || r.pulses != w.Count || r.opens != w.Opens || s.Noise != w.Noise ||
		s.Short != w.Short || s.Glitches != w.Glitches || s.Lost != w.Lost {
		t.Errorf("count %d (%d pulse events), %d opens, noise %d, short %d, glitches %d, lost %d; want %+v",
			s.Count, r.pulses, r.opens, s.Noise, s.Short, s.Glitches, s.Lost, w)
	}
	for ts, want := range r.exp.LastClosedAtOpen {
		if got, ok := r.closedAt[stamp(t, ts)]; !ok || got != time.Duration(want) {
			t.Errorf("LastClosed at the open %s = %v (reported %v), want %v", ts, got, ok, time.Duration(want))
		}
	}
	for ts, want := range r.exp.LastIntervalAtPulse {
		if got, ok := r.intervalAt[stamp(t, ts)]; !ok || got != time.Duration(want) {
			t.Errorf("LastInterval at the pulse %s = %v (reported %v), want %v", ts, got, ok, time.Duration(want))
		}
	}
}

// The same night as the kernel debounce should deliver it: the spikes never
// reach user space.
func TestReplayFirstNightDebounced(t *testing.T) {
	r := loadReplay(t, firstNight)
	var clean []Edge
	prev := r.level
	for _, e := range r.edges {
		if e.Level == prev {
			continue // a spike: the kernel would have reported nothing
		}
		prev = e.Level
		e.Seqno = uint64(len(clean) + 1)
		clean = append(clean, e)
	}
	r.run(t, clean)
	s, w := r.k.Snapshot(), r.exp.WantDebounced
	if s.Count != w.Count || r.opens != w.Opens || s.Noise != w.Noise || s.Short != 0 || s.Glitches != 0 {
		t.Errorf("snapshot %+v, %d opens; want %+v", s, r.opens, w)
	}
}

// Without the user-space rules (hold 0, no backstop) the same-level rule
// alone still gets the right count: it's the state machine that does the work.
func TestReplayFirstNightStateMachineAlone(t *testing.T) {
	r := loadReplay(t, firstNight)
	r.cfg = Config{}
	r.run(t, r.edges)
	if s := r.k.Snapshot(); s.Count != r.exp.Want.Count {
		t.Errorf("count = %d, want %d", s.Count, r.exp.Want.Count)
	}
}

func TestLevelString(t *testing.T) {
	if Open.String() != "open" || Closed.String() != "closed" || Level(0).String() != "Level(0)" {
		t.Errorf("%v %v %v", Open, Closed, Level(0))
	}
}
