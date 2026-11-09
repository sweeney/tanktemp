package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
	rt "github.com/sweeney/tanktemp/internal/reading/readingtest"
)

// The adaptive cadence end to end (issue #1, review on #4): the real run,
// with lifecycle's Next driving the sweeps on production timers
// (Now: time.Now, so time.NewTimer), inside a testing/synctest bubble.
// Time is virtual and exact: a sweep can take time (a Source that sleeps),
// the wall clock can step on its own (deps.Wall), and synctest.Test fails
// if any goroutine is left behind when run returns.
//
// Each scenario is a row: what the probes read sweep by sweep, how long
// each sweep takes, what goes wrong when, and what the gaps between
// sweeps and the verbose log must show. A new corner case is a new row.

// scenario is one row.
type scenario struct {
	name     string
	fast     *config.Fast // nil: no sweep.fast
	verbose  bool
	bottom   func(i int) reading.Reading       // the bottom probe in sweep i; the top reads 61.375 throughout
	took     func(i int) time.Duration         // virtual time sweep i takes; nil: none
	discover map[int]bool                      // discovery fails in sweep i
	wall     map[int]time.Duration             // step the wall clock during sweep i (it is stamped already: i+1 shows it)
	sigterm  int                               // SIGTERM during sweep n (0: only at the end)
	runFor   time.Duration                     // then SIGTERM, if not already
	gaps     []gapRule                         // the gaps between sweep starts, in order
	lines    []string                          // verbose lines that must appear, in this order
	never    []string                          // substrings the log must not contain
	check    func(t *testing.T, r scenarioRun) // anything else
}

// scenarioRun is what a scenario produced.
type scenarioRun struct {
	starts []time.Time // each sweep's start, on the bubble's (monotonic) clock
	gaps   []time.Duration
	logs   string
	msgs   []mqttpub.Message
	err    error
}

// gapRule matches some of the gaps.
type gapRule struct {
	n       int           // how many gaps; for decay, at least this many
	exact   time.Duration // each is exactly this...
	decay   bool          // ...or: each rises from the last, between fast and slow, until slow
	rest    bool          // this rule takes every gap left
	comment string
}

func exactly(d time.Duration, n int) gapRule { return gapRule{n: n, exact: d} }
func easing(atLeast int) gapRule             { return gapRule{n: atLeast, decay: true} }
func thenAll(d time.Duration) gapRule        { return gapRule{exact: d, rest: true} }

// draw is the bottom probe through a hot-water draw: 40 °C for two sweeps,
// then 36 °C (a 4 °C fall in 30 s: 8 °C/min), and it stays there.
func draw(i int) reading.Reading {
	if i < 2 {
		return rt.OK(40000)
	}
	return rt.OK(36000)
}

// then is draw, with sweep at replaced.
func then(at int, r reading.Reading) func(int) reading.Reading {
	return func(i int) reading.Reading {
		if i == at {
			return r
		}
		return draw(i)
	}
}

func fastDefaults() *config.Fast { f := config.DefaultFast; return &f } // 10s, 1.5 °C/min, 2m, 5m

// theDrawsGaps: 30 s twice, the trigger at sweep 2, then 13 gaps of 10 s
// (12 in the hold, and the decay's first instant, which is exactly fast),
// then easing out to 30 s.
var theDrawsGaps = []gapRule{exactly(30*time.Second, 2), exactly(10*time.Second, 13), easing(15), thenAll(30 * time.Second)}

func TestAdaptiveCadenceScenarios(t *testing.T) {
	scenarios := []scenario{
		{
			name: "a draw: fast at once, hold, ease back, slow",
			fast: fastDefaults(), verbose: true, bottom: draw, runFor: 15 * time.Minute,
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)", "cadence: decay 10s", "cadence: slow 30s"},
		},
		{
			name:    "no sweep.fast: the same draw changes nothing",
			verbose: true, bottom: draw, runFor: 5 * time.Minute,
			gaps:  []gapRule{thenAll(30 * time.Second)},
			never: []string{"cadence:"},
		},
		{
			name: "verbose off: the same cadence, and no cadence lines",
			fast: fastDefaults(), bottom: draw, runFor: 15 * time.Minute,
			gaps:  theDrawsGaps,
			never: []string{"cadence:"},
		},
		{
			name: "an overrun in fast mode skips ticks onto the 10 s grid, never queues",
			fast: fastDefaults(), verbose: true, bottom: draw, runFor: 4 * time.Minute,
			took: func(i int) time.Duration {
				if i >= 3 && i <= 5 {
					return 12 * time.Second // longer than fast.interval
				}
				return 0
			},
			// Sweep 3 starts at +70 and ends at +82: +80 has passed, so the
			// next is +90. The same for sweeps 4 and 5.
			gaps: []gapRule{exactly(30*time.Second, 2), exactly(10*time.Second, 1), exactly(20*time.Second, 3), exactly(10*time.Second, 6)},
		},
		{
			name: "a probe failing, then recovering at a new value, mid-draw: no second trigger",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			bottom: func(i int) reading.Reading {
				switch {
				case i == 4 || i == 5:
					return rt.Fail(reading.CRC)
				case i >= 6:
					return rt.OK(30000) // 6 °C lower: but there's no OK reading just before
				}
				return draw(i)
			},
			gaps: theDrawsGaps,
			check: func(t *testing.T, r scenarioRun) {
				if n := strings.Count(r.logs, "cadence: fast"); n != 1 {
					t.Errorf("%d triggers, want only the draw's", n)
				}
			},
		},
		{
			name: "a suspect 85 °C mid-draw triggers nothing, going or coming back",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			bottom: then(5, rt.Suspect(85000)),
			gaps:   theDrawsGaps,
			check: func(t *testing.T, r scenarioRun) {
				if n := strings.Count(r.logs, "cadence: fast"); n != 1 {
					t.Errorf("%d triggers, want only the draw's", n)
				}
			},
		},
		{
			name: "a discovery error mid-hold: every probe unusable, then nothing to compare with",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			discover: map[int]bool{5: true},
			bottom: func(i int) reading.Reading {
				if i >= 6 {
					return rt.OK(30000) // a fall across the failed sweep: no previous reading
				}
				return draw(i)
			},
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s", "sweep: 0 ok, 2 failed, 0s: discovery failed: no 1-Wire bus masters; not_present: hot-water-cylinder/bottom, hot-water-cylinder/top", "cadence: decay 10s"},
			check: func(t *testing.T, r scenarioRun) {
				if n := strings.Count(r.logs, "cadence: fast"); n != 1 {
					t.Errorf("%d triggers, want only the draw's", n)
				}
			},
		},
		{
			name: "the wall clock steps back an hour in the hold and forward two in the decay: the cadence doesn't notice",
			fast: fastDefaults(), verbose: true, bottom: draw, runFor: 15 * time.Minute,
			wall:  map[int]time.Duration{5: -time.Hour, 20: 2 * time.Hour},
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)", "cadence: decay 10s", "cadence: slow 30s"},
			check: func(t *testing.T, r scenarioRun) {
				// The published timestamps did step: they're the wall clock.
				ts := stateTimestamps(t, r.msgs)
				if len(ts) < 22 || !ts[6].Before(ts[5]) || ts[21].Sub(ts[20]) < time.Hour {
					t.Errorf("published timestamps %v: want a step back at 6 and forward at 21 (the first sweeps stamped after each)", ts)
				}
			},
		},
		{
			name: "SIGTERM during a fast sweep: run returns, SHUTDOWN is last, the sweep isn't published",
			fast: fastDefaults(), verbose: true, bottom: draw, runFor: 5 * time.Minute,
			took: func(i int) time.Duration {
				if i == 4 {
					return 5 * time.Second
				}
				return 0
			},
			sigterm: 4,
			check: func(t *testing.T, r scenarioRun) {
				if r.err != nil {
					t.Errorf("run returned %v", r.err)
				}
				if got := len(stateTimestamps(t, r.msgs)); got != 4 {
					t.Errorf("%d states published, want 4 (sweeps 0 to 3)", got)
				}
				if last := r.msgs[len(r.msgs)-1]; event(t, last) != "SHUTDOWN" {
					t.Errorf("last message %s, want SHUTDOWN", last.Payload)
				}
				if len(r.starts) != 5 {
					t.Errorf("%d sweeps started, want 5: none after the signal", len(r.starts))
				}
			},
		},
		{
			name:    "hold: 0s goes straight into the decay",
			fast:    &config.Fast{Interval: 10 * time.Second, TriggerCPerMin: 1.5, Hold: 0, Decay: 5 * time.Minute},
			verbose: true, bottom: draw, runFor: 10 * time.Minute,
			gaps:  []gapRule{exactly(30*time.Second, 2), exactly(10*time.Second, 1), easing(15), thenAll(30 * time.Second)},
			lines: []string{"cadence: fast 10s", "cadence: slow 30s"},
		},
		{
			name: "a second draw mid-decay snaps back to fast",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			bottom: func(i int) reading.Reading {
				if i >= 18 {
					return rt.OK(32000) // sweep 18 is in the decay
				}
				return draw(i)
			},
			lines: []string{"cadence: fast 10s", "cadence: decay 10s", "cadence: fast 10s", "cadence: slow 30s"},
			check: func(t *testing.T, r scenarioRun) {
				if len(r.gaps) < 31 || r.gaps[17] <= 10*time.Second || r.gaps[18] != 10*time.Second {
					t.Fatalf("gaps %v: want decaying before sweep 18, then 10 s", r.gaps)
				}
				for i := 18; i < 31; i++ { // a whole new hold (and the decay's first instant)
					if r.gaps[i] != 10*time.Second {
						t.Errorf("gap %d = %v, want 10s in the second hold", i, r.gaps[i])
					}
				}
			},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := runScenario(t, sc)
				checkGaps(t, r.gaps, sc.gaps, sc.fast)
				checkLines(t, r.logs, sc.lines, sc.never)
				if sc.check != nil {
					sc.check(t, r)
				}
			})
		})
	}
}

// runScenario runs tanktemp through sc, inside the caller's synctest bubble.
func runScenario(t *testing.T, sc scenario) scenarioRun {
	t.Helper()
	cfg := testConfig(t)
	cfg.Sweep.Fast = sc.fast
	sig := make(chan os.Signal, 1)
	src := &scenarioSource{sc: sc, sig: sig, clock: &wallClock{}}
	pub := &mqttpubtest.Fake{}
	var logs syncBuf
	logger := log.New(&logs, "", 0)
	d := deps{
		Source: src, Pub: pub,
		Synced: func() bool { return true },
		Now:    time.Now, Wall: src.clock.Now, // production timers; the wall clock can step alone
		Signals: sig, Logger: logger,
	}
	if sc.verbose {
		d.Verbose = logger
	}
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), cfg, d) }()
	time.Sleep(sc.runFor)
	synctest.Wait()
	if sc.sigterm == 0 {
		sig <- syscall.SIGTERM
	}
	err := <-done

	starts := src.Starts()
	var gaps []time.Duration
	for i := 1; i < len(starts); i++ {
		gaps = append(gaps, starts[i].Sub(starts[i-1]))
	}
	return scenarioRun{starts: starts, gaps: gaps, logs: logs.String(), msgs: pub.Snapshot(), err: err}
}

// checkGaps matches gaps against the rules, in order.
func checkGaps(t *testing.T, gaps []time.Duration, rules []gapRule, fast *config.Fast) {
	t.Helper()
	i := 0
	for _, r := range rules {
		switch {
		case r.rest:
			for ; i < len(gaps); i++ {
				if gaps[i] != r.exact {
					t.Errorf("gap %d = %v, want %v from here on (gaps %v)", i, gaps[i], r.exact, gaps)
					return
				}
			}
		case r.decay:
			first := i
			for ; i < len(gaps) && gaps[i] < 30*time.Second; i++ {
				if gaps[i] <= gaps[i-1] || gaps[i] < fast.Interval {
					t.Errorf("gap %d = %v after %v: want rising through the decay (gaps %v)", i, gaps[i], gaps[i-1], gaps)
					return
				}
			}
			if i-first < r.n {
				t.Errorf("%d gaps in the decay, want at least %d (gaps %v)", i-first, r.n, gaps)
				return
			}
		default:
			for k := 0; k < r.n; k, i = k+1, i+1 {
				if i >= len(gaps) || gaps[i] != r.exact {
					t.Errorf("gap %d: want %v (gaps %v)", i, r.exact, gaps)
					return
				}
			}
		}
	}
}

// checkLines: want appear in the log in this order (other lines between
// are fine), and none of never appears.
func checkLines(t *testing.T, logs string, want, never []string) {
	t.Helper()
	rest := logs
	for _, w := range want {
		k := strings.Index(rest, w)
		if k < 0 {
			t.Errorf("log lacks %q after the lines before it:\n%s", w, logs)
			return
		}
		rest = rest[k+len(w):]
	}
	for _, n := range never {
		if strings.Contains(logs, n) {
			t.Errorf("log contains %q:\n%s", n, logs)
		}
	}
}

func stateTimestamps(t *testing.T, msgs []mqttpub.Message) []time.Time {
	t.Helper()
	var out []time.Time
	for _, m := range msgs {
		if !strings.HasSuffix(m.Topic, "/state") {
			continue
		}
		const key = `"timestamp":"`
		k := strings.Index(m.Payload, key)
		if k < 0 {
			t.Fatalf("no timestamp in %s", m.Payload)
		}
		at, err := time.Parse(time.RFC3339, m.Payload[k+len(key):k+len(key)+len("2006-01-02T15:04:05Z")])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, at)
	}
	return out
}

// wallClock is the bubble's clock plus an offset the scenario can step, as
// NTP steps the wall clock while the monotonic clock carries on.
type wallClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (w *wallClock) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Now().Add(w.off)
}

func (w *wallClock) step(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.off += d
}

// scenarioSource is a reading.Source scripted by sweep: each Discover
// after the startup checks' one starts a sweep. It records the sweep's start on the bubble's clock,
// applies the scenario's wall step, signal and discovery failure, and Read
// takes the sweep's virtual time (cancellable, as the real one is).
type scenarioSource struct {
	sc    scenario
	sig   chan os.Signal
	clock *wallClock

	mu      sync.Mutex
	checked bool        // startup.Check's Discover has happened
	starts  []time.Time // each sweep's Discover
}

var errNoMasters = errors.New("no 1-Wire bus masters")

func (s *scenarioSource) sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts) - 1
}

func (s *scenarioSource) Starts() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.starts...)
}

func (s *scenarioSource) Discover(ctx context.Context) (reading.Topology, error) {
	s.mu.Lock()
	if !s.checked {
		// run's startup checks discover once before the first sweep.
		s.checked = true
		s.mu.Unlock()
		return reading.NewTopology(map[string][]string{"w1_bus_master1": {top, bottom}}), nil
	}
	i := len(s.starts)
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	if d, ok := s.sc.wall[i]; ok {
		s.clock.step(d)
	}
	if s.sc.sigterm != 0 && s.sc.sigterm == i {
		s.sig <- syscall.SIGTERM
	}
	if s.sc.discover[i] {
		return reading.Topology{}, errNoMasters
	}
	return reading.NewTopology(map[string][]string{"w1_bus_master1": {top, bottom}}), nil
}

func (s *scenarioSource) Read(ctx context.Context, t reading.Topology, roms []string) map[string]reading.Reading {
	i := s.sweep()
	if s.sc.took != nil {
		select {
		case <-time.After(s.sc.took(i)):
		case <-ctx.Done():
		}
	}
	out := map[string]reading.Reading{}
	for _, rom := range roms {
		var r reading.Reading
		bus, present := t.Bus(rom)
		switch {
		case ctx.Err() != nil:
			out[rom] = reading.Reading{ROM: rom, Code: reading.Cancelled}
			continue
		case !present:
			r = rt.Fail(reading.NotPresent)
		case rom == bottom:
			r = s.sc.bottom(i)
		default:
			r = rt.OK(61375)
		}
		r.ROM, r.Bus, r.Attempts = rom, bus, 1
		if r.MilliC != nil {
			v := *r.MilliC
			r.MilliC = &v
		}
		out[rom] = r
	}
	return out
}

func (s *scenarioSource) Inspect(context.Context, string) (reading.Info, error) {
	return reading.Info{ExtPower: rt.Bool(true), Resolution: rt.Int(12)}, nil
}

var _ reading.Source = (*scenarioSource)(nil)
