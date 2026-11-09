package main

import (
	"context"
	"errors"
	"fmt"
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
// Each scenario is a row: what each probe reads sweep by sweep, how long
// each sweep takes, what goes wrong when, and what the gaps between
// sweeps and the verbose log must show. A new corner case is a new row;
// checkGaps fails on any gap a row's rules don't account for.

// scenario is one row.
type scenario struct {
	name    string
	fast    *config.Fast         // nil: no sweep.fast
	cfg     func(*config.Config) // optional: more tanks, another interval; after fast is set
	verbose bool
	// probes scripts each probe by ROM: its reading in sweep i. A ROM with
	// no script reads a steady 61.375 °C. A script returning not_present
	// takes the probe off the bus for that sweep (it's left out of the
	// topology), as unplugging it would.
	probes   map[string]func(i int) reading.Reading
	took     func(i int) time.Duration         // virtual time sweep i takes; nil: none
	discover map[int]bool                      // discovery fails in sweep i
	wall     map[int]time.Duration             // step the wall clock during sweep i (it is stamped already: i+1 shows it)
	sigterm  *int                              // SIGTERM during sweep n (nil: only at the end; during(0) is the immediate sweep)
	runFor   time.Duration                     // then SIGTERM, if not already
	gaps     []gapRule                         // every gap between sweep starts, in order
	lines    []string                          // verbose lines that must appear, in this order
	never    []string                          // substrings the log must not contain
	check    func(t *testing.T, r scenarioRun) // anything else
}

func during(i int) *int { return &i }

// scenarioRun is what a scenario produced.
type scenarioRun struct {
	starts []time.Time // each sweep's start, on the bubble's (monotonic) clock
	gaps   []time.Duration
	logs   string
	msgs   []mqttpub.Message
	err    error
}

// gapRule matches some of the gaps. checkGaps applies a row's rules in
// order and fails if any gap is left over, so a row says what every gap
// is, or ends with anyAfter() to say it doesn't care.
type gapRule struct {
	kind  gapKind
	n     int           // exact, easing: how many; easingToSlow: at least
	exact time.Duration // exact, rest
}

type gapKind int

const (
	gapExact        gapKind = iota // n gaps of exactly d
	gapEasing                      // n gaps easing up: each at least fast and the one before, under slow
	gapEasingToSlow                // gaps easing up until one is slow: at least n of them, and slow must follow
	gapRest                        // every gap left is exactly d
	gapAny                         // every gap left, whatever it is
)

func exactly(d time.Duration, n int) gapRule { return gapRule{kind: gapExact, n: n, exact: d} }
func easing(n int) gapRule                   { return gapRule{kind: gapEasing, n: n} }
func easingToSlow(atLeast int) gapRule       { return gapRule{kind: gapEasingToSlow, n: atLeast} }
func thenAll(d time.Duration) gapRule        { return gapRule{kind: gapRest, exact: d} }
func anyAfter() gapRule                      { return gapRule{kind: gapAny} }

// draw is the bottom probe through a hot-water draw: 40 °C for two sweeps,
// then 36 °C (a 4 °C fall in 30 s: 8 °C/min), and it stays there.
func draw(i int) reading.Reading {
	if i < 2 {
		return rt.OK(40000)
	}
	return rt.OK(36000)
}

// drawThen is draw, with sweep at replaced by r.
func drawThen(at int, r reading.Reading) func(int) reading.Reading {
	return func(i int) reading.Reading {
		if i == at {
			return r
		}
		return draw(i)
	}
}

// from reads before until sweep at, then after.
func from(at int, before, after int) func(int) reading.Reading {
	return func(i int) reading.Reading {
		if i < at {
			return rt.OK(before)
		}
		return rt.OK(after)
	}
}

func fastDefaults() *config.Fast { f := config.DefaultFast; return &f } // 10s, 1.5 °C/min, 2m, 5m

const (
	s10 = 10 * time.Second
	s30 = 30 * time.Second
)

// theDrawsGaps: 30 s twice, the trigger at sweep 2, then 13 gaps of 10 s
// (12 in the hold, and the decay's first instant, which is exactly fast),
// then easing out (18 gaps), then 30 s.
var theDrawsGaps = []gapRule{exactly(s30, 2), exactly(s10, 13), easingToSlow(18), thenAll(s30)}

// other is a third probe, on a second tank some rows add.
const other = "28-000000000003"

func withSecondTank(c *config.Config) {
	c.Tanks = append(c.Tanks, config.Tank{DeviceID: "second", Sensors: []config.Sensor{
		{ROM: other, Position: "middle", Order: 1},
	}})
}

func TestAdaptiveCadenceScenarios(t *testing.T) {
	onlyTheDraw := func(t *testing.T, r scenarioRun) {
		if n := strings.Count(r.logs, "cadence: fast"); n != 1 {
			t.Errorf("%d triggers, want only the draw's", n)
		}
	}
	scenarios := []scenario{
		{
			name: "a draw: fast at once, hold, ease back, slow",
			fast: fastDefaults(), verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 15 * time.Minute,
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)", "cadence: decay 10s", "cadence: slow 30s"},
		},
		{
			name:    "no sweep.fast: the same draw changes nothing",
			verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 5 * time.Minute,
			gaps:  []gapRule{thenAll(s30)},
			never: []string{"cadence:"},
		},
		{
			name: "verbose off: the same cadence, and no cadence lines",
			fast: fastDefaults(), probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 15 * time.Minute,
			gaps:  theDrawsGaps,
			never: []string{"cadence:"},
		},
		{
			name: "a longer slow interval: the decay eases out to it, not to 30 s",
			fast: fastDefaults(), cfg: func(c *config.Config) { c.Sweep.Interval = time.Minute },
			// The fall is 6 °C in 60 s, 6 °C/min.
			verbose: true, probes: map[string]func(int) reading.Reading{bottom: from(2, 40000, 34000)}, runFor: 20 * time.Minute,
			gaps:  []gapRule{exactly(time.Minute, 2), exactly(s10, 13), easingToSlow(10), thenAll(time.Minute)},
			lines: []string{"cadence: fast 10s", "cadence: slow 1m0s"},
		},
		{
			name: "an overrun in fast mode skips onto the 10 s grid, never queues",
			fast: fastDefaults(), verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 15 * time.Minute,
			took: func(i int) time.Duration {
				if i >= 3 && i <= 5 {
					return 12 * time.Second // longer than fast.interval
				}
				return 0
			},
			// Sweep 3 starts at +70 and ends at +82: +80 has passed, so the
			// next is +90. The same for sweeps 4 and 5. Then the rest of
			// the hold, which ends at +180, the decay and slow.
			gaps: []gapRule{exactly(s30, 2), exactly(s10, 1), exactly(20*time.Second, 3), exactly(s10, 6), easingToSlow(18), thenAll(s30)},
		},
		{
			name: "a sweep ending exactly on the next due time: the next starts at once, nothing skipped",
			fast: fastDefaults(), verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 15 * time.Minute,
			took: func(i int) time.Duration {
				if i == 3 {
					return s10 // exactly fast.interval
				}
				return 0
			},
			// Sweep 3 starts at +70 and ends at +80, exactly when sweep 4
			// is due: it starts then, so the gaps are the plain draw's.
			// Before review on #4 this gap was 20 s, and a tick skipped.
			gaps: theDrawsGaps,
		},
		{
			name: "two probes on one tank move at once: the faster is the trigger",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			probes: map[string]func(int) reading.Reading{
				bottom: draw,                  // -8 °C/min
				top:    from(2, 61375, 63375), // +4 °C/min
			},
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)"},
			check: onlyTheDraw,
		},
		{
			name: "a fall on one tank, then a rise on another: each triggers, and the hold runs from the second",
			fast: fastDefaults(), cfg: withSecondTank, verbose: true, runFor: 15 * time.Minute,
			probes: map[string]func(int) reading.Reading{
				bottom: draw,                  // sweep 2, at +60: -8 °C/min
				other:  from(3, 50000, 52000), // sweep 3, 10 s later: +12 °C/min
			},
			gaps: []gapRule{exactly(s30, 2), exactly(s10, 14), easingToSlow(18), thenAll(s30)},
			lines: []string{
				"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)",
				"cadence: fast 10s (second/middle +12.0 °C/min)",
				"cadence: decay 10s", "cadence: slow 30s",
			},
		},
		{
			name: "a probe failing, then recovering at a new value, mid-draw: no second trigger",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			probes: map[string]func(int) reading.Reading{bottom: func(i int) reading.Reading {
				switch {
				case i == 4 || i == 5:
					return rt.Fail(reading.CRC)
				case i >= 6:
					return rt.OK(30000) // 6 °C lower: but there's no OK reading just before
				}
				return draw(i)
			}},
			gaps:  theDrawsGaps,
			check: onlyTheDraw,
		},
		{
			name: "a probe dropping off the bus mid-hold, and back at a new value: no second trigger",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			probes: map[string]func(int) reading.Reading{bottom: func(i int) reading.Reading {
				switch {
				case i == 5:
					return rt.Fail(reading.NotPresent) // unplugged: not on the bus
				case i >= 6:
					return rt.OK(30000)
				}
				return draw(i)
			}},
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s", "sweep: 1 ok, 1 failed, 0s: not_present: hot-water-cylinder/bottom", "cadence: decay 10s"},
			check: onlyTheDraw,
		},
		{
			name: "a suspect 85 °C mid-draw triggers nothing, going or coming back",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			probes: map[string]func(int) reading.Reading{bottom: drawThen(5, rt.Suspect(85000))},
			gaps:   theDrawsGaps,
			check:  onlyTheDraw,
		},
		{
			name: "a discovery error mid-hold: every probe unusable, then nothing to compare with",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			discover: map[int]bool{5: true},
			// From sweep 6 on, 6 °C lower: a fall across the failed sweep,
			// with no previous reading to compare it with.
			probes: map[string]func(int) reading.Reading{bottom: func(i int) reading.Reading {
				if i >= 6 {
					return rt.OK(30000)
				}
				return draw(i)
			}},
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s", "sweep: 0 ok, 2 failed, 0s: discovery failed: no 1-Wire bus masters; not_present: hot-water-cylinder/bottom, hot-water-cylinder/top", "cadence: decay 10s"},
			check: onlyTheDraw,
		},
		{
			name: "the wall clock steps back an hour in the hold and forward two in the decay: the cadence doesn't notice",
			fast: fastDefaults(), verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 15 * time.Minute,
			wall:  map[int]time.Duration{5: -time.Hour, 20: 2 * time.Hour},
			gaps:  theDrawsGaps,
			lines: []string{"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)", "cadence: decay 10s", "cadence: slow 30s"},
			check: func(t *testing.T, r scenarioRun) {
				// The published timestamps did step: they're the wall clock,
				// and sweeps 6 and 21 are the first stamped after each step.
				ts := stateTimestamps(t, r.msgs)
				if len(ts) < 22 || !ts[6].Before(ts[5]) || ts[21].Sub(ts[20]) < time.Hour {
					t.Errorf("published timestamps %v: want a step back at 6 and forward at 21 (the first sweeps stamped after each)", ts)
				}
			},
		},
		{
			name: "SIGTERM during a fast sweep: run returns, SHUTDOWN is last, the sweep isn't published",
			fast: fastDefaults(), verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 5 * time.Minute,
			took: func(i int) time.Duration {
				if i == 4 {
					return 5 * time.Second
				}
				return 0
			},
			sigterm: during(4),
			gaps:    []gapRule{exactly(s30, 2), exactly(s10, 2)},
			check: func(t *testing.T, r scenarioRun) {
				checkShutdown(t, r, 4)
			},
		},
		{
			name: "SIGTERM during the immediate sweep, the one that starts without Next",
			fast: fastDefaults(), verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: time.Minute,
			took:    func(int) time.Duration { return 5 * time.Second },
			sigterm: during(0),
			gaps:    nil, // one sweep: no gaps at all
			check: func(t *testing.T, r scenarioRun) {
				checkShutdown(t, r, 0)
			},
		},
		{
			name:    "hold: 0s goes straight into the decay",
			fast:    &config.Fast{Interval: s10, TriggerCPerMin: 1.5, Hold: 0, Decay: 5 * time.Minute},
			verbose: true, probes: map[string]func(int) reading.Reading{bottom: draw}, runFor: 10 * time.Minute,
			gaps:  []gapRule{exactly(s30, 2), exactly(s10, 1), easingToSlow(18), thenAll(s30)},
			lines: []string{"cadence: fast 10s", "cadence: slow 30s"},
		},
		{
			name: "a second draw mid-decay snaps back to fast for a whole new hold",
			fast: fastDefaults(), verbose: true, runFor: 15 * time.Minute,
			// The draw at sweep 2, then a second fall at sweep 18, three
			// gaps into the decay: 4 °C in about 10.8 s.
			probes: map[string]func(int) reading.Reading{bottom: func(i int) reading.Reading {
				if i >= 18 {
					return rt.OK(32000)
				}
				return draw(i)
			}},
			gaps:  []gapRule{exactly(s30, 2), exactly(s10, 13), easing(3), exactly(s10, 13), easingToSlow(18), thenAll(s30)},
			lines: []string{"cadence: fast 10s", "cadence: decay 10s", "cadence: fast 10s", "cadence: decay 10s", "cadence: slow 30s"},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := scenarioConfig(t, sc)
				r := runScenario(t, sc, cfg)
				fast := time.Duration(0)
				if cfg.Sweep.Fast != nil {
					fast = cfg.Sweep.Fast.Interval
				}
				checkGaps(t, r.gaps, sc.gaps, fast, cfg.Sweep.Interval)
				checkLines(t, r.logs, sc.lines, sc.never)
				if sc.check != nil {
					sc.check(t, r)
				}
			})
		})
	}
}

// checkShutdown: run returned nil, SHUTDOWN is the last message, sweeps 0
// to n-1 were published and sweep n (cancelled) wasn't, and no sweep
// started after it.
func checkShutdown(t *testing.T, r scenarioRun, n int) {
	t.Helper()
	if r.err != nil {
		t.Errorf("run returned %v", r.err)
	}
	if got := len(stateTimestamps(t, r.msgs)); got != n {
		t.Errorf("%d states published, want %d", got, n)
	}
	if len(r.msgs) == 0 || event(t, r.msgs[len(r.msgs)-1]) != "SHUTDOWN" {
		t.Errorf("last message isn't SHUTDOWN: %v", r.msgs)
	}
	if len(r.starts) != n+1 {
		t.Errorf("%d sweeps started, want %d: none after the signal", len(r.starts), n+1)
	}
}

// scenarioConfig is testConfig with the row's fast block and mutator.
func scenarioConfig(t *testing.T, sc scenario) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Sweep.Fast = sc.fast
	if sc.cfg != nil {
		sc.cfg(&cfg)
	}
	return cfg
}

// runScenario runs tanktemp through sc, inside the caller's synctest bubble.
func runScenario(t *testing.T, sc scenario, cfg config.Config) scenarioRun {
	t.Helper()
	sig := make(chan os.Signal, 1)
	var roms []string
	for _, tk := range cfg.Tanks {
		for _, x := range tk.Sensors {
			roms = append(roms, x.ROM)
		}
	}
	src := &scenarioSource{sc: sc, roms: roms, sig: sig, clock: &wallClock{}}
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
	if sc.sigterm == nil {
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

// checkGaps fails t with every problem gapProblems finds.
func checkGaps(t *testing.T, gaps []time.Duration, rules []gapRule, fast, slow time.Duration) {
	t.Helper()
	for _, p := range gapProblems(gaps, rules, fast, slow) {
		t.Errorf("%s (gaps %v)", p, gaps)
	}
}

// gapProblems matches every gap against the rules, in order, and reports
// any gap the rules leave over (review on #4: a row must say what every
// gap is, or end with anyAfter). fast and slow are the row's config. It
// stops at the first problem, since every later match would be shifted.
// It's pure so TestGapProblems can check the matcher itself.
func gapProblems(gaps []time.Duration, rules []gapRule, fast, slow time.Duration) []string {
	i := 0
	// easingOK: gaps[i] eases up from prev: at least fast and the gap
	// before it in this rule, and under slow. Non-decreasing, not strictly
	// rising: each interval is rounded to the millisecond, so a long decay
	// can give two equal gaps in a row.
	easingOK := func(prev time.Duration) bool {
		return gaps[i] >= fast && gaps[i] >= prev && gaps[i] < slow
	}
	for _, r := range rules {
		switch r.kind {
		case gapExact:
			for k := 0; k < r.n; k, i = k+1, i+1 {
				if i >= len(gaps) || gaps[i] != r.exact {
					return []string{fmt.Sprintf("gap %d: want %v, the %d of %d", i, r.exact, k+1, r.n)}
				}
			}
		case gapEasing:
			prev := fast
			for k := 0; k < r.n; k, i = k+1, i+1 {
				if i >= len(gaps) || !easingOK(prev) {
					return []string{fmt.Sprintf("gap %d: want easing up from %v, under %v", i, prev, slow)}
				}
				prev = gaps[i]
			}
		case gapEasingToSlow:
			first, prev := i, fast
			for ; i < len(gaps) && gaps[i] < slow; i++ {
				if !easingOK(prev) {
					return []string{fmt.Sprintf("gap %d = %v: want easing up from %v", i, gaps[i], prev)}
				}
				prev = gaps[i]
			}
			switch {
			case i-first < r.n:
				return []string{fmt.Sprintf("%d gaps easing out, want at least %d", i-first, r.n)}
			case i >= len(gaps):
				return []string{fmt.Sprintf("the decay never reached the slow interval, %v", slow)}
			}
		case gapRest:
			for ; i < len(gaps); i++ {
				if gaps[i] != r.exact {
					return []string{fmt.Sprintf("gap %d = %v, want %v from here on", i, gaps[i], r.exact)}
				}
			}
		case gapAny:
			i = len(gaps)
		}
	}
	if i < len(gaps) {
		return []string{fmt.Sprintf("%d gaps the rules don't account for, from gap %d: %v", len(gaps)-i, i, gaps[i:])}
	}
	return nil
}

// The matcher itself (review on #4): each of the faults the review found
// in the first version, and that it still passes what it should.
func TestGapProblems(t *testing.T) {
	ms := func(n ...int) []time.Duration {
		var out []time.Duration
		for _, v := range n {
			out = append(out, time.Duration(v)*time.Millisecond)
		}
		return out
	}
	tests := []struct {
		name        string
		gaps        []time.Duration
		rules       []gapRule
		slow        time.Duration
		wantProblem string // "" for none; else a substring
	}{
		{"all accounted for", ms(30000, 10000, 10373, 30000), []gapRule{exactly(s30, 1), exactly(s10, 1), easingToSlow(1), thenAll(s30)}, s30, ""},
		{"trailing gaps unaccounted for fail", ms(30000, 10000, 25000), []gapRule{exactly(s30, 1), exactly(s10, 1)}, s30, "1 gaps the rules don't account for"},
		{"anyAfter accounts for them", ms(30000, 10000, 25000), []gapRule{exactly(s30, 1), anyAfter()}, s30, ""},
		{"no rules and no gaps", nil, nil, s30, ""},
		{"no rules but gaps fail", ms(30000), nil, s30, "1 gaps the rules don't account for"},
		{"the slow interval is the row's, not 30 s", ms(10000, 40000, 60000), []gapRule{exactly(s10, 1), easingToSlow(1), thenAll(time.Minute)}, time.Minute, ""},
		{"easing first doesn't panic", ms(10500, 11000), []gapRule{easing(2)}, s30, ""},
		{"easing first below fast fails", ms(9000), []gapRule{easing(1)}, s30, "easing up from 10s"},
		{"equal adjacent decay gaps are fine", ms(10001, 10001, 10002, 30000), []gapRule{easingToSlow(3), thenAll(s30)}, s30, ""},
		{"a falling decay gap fails", ms(12000, 11000, 30000), []gapRule{easingToSlow(1), thenAll(s30)}, s30, "want easing up from 12s"},
		{"a decay that never reaches slow fails", ms(10500, 11000), []gapRule{easingToSlow(1)}, s30, "never reached the slow interval"},
		{"too few exact gaps fail", ms(10000), []gapRule{exactly(s10, 2)}, s30, "the 2 of 2"},
	}
	for _, tt := range tests {
		got := gapProblems(tt.gaps, tt.rules, s10, tt.slow)
		switch {
		case tt.wantProblem == "" && len(got) > 0:
			t.Errorf("%s: problems %q, want none", tt.name, got)
		case tt.wantProblem != "" && (len(got) == 0 || !strings.Contains(got[0], tt.wantProblem)):
			t.Errorf("%s: problems %q, want one mentioning %q", tt.name, got, tt.wantProblem)
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
		if !strings.HasSuffix(m.Topic, "/state") || !strings.Contains(m.Topic, "/hot-water-cylinder/") {
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
// after the startup checks' one starts a sweep. It records the sweep's
// start on the bubble's clock, applies the row's wall step, signal and
// discovery failure, and leaves off the bus any probe whose script says
// not_present. Read takes the sweep's virtual time (cancellable, as the
// real one is).
type scenarioSource struct {
	sc    scenario
	roms  []string // every configured ROM
	sig   chan os.Signal
	clock *wallClock

	mu      sync.Mutex
	checked bool        // startup.Check's Discover has happened
	starts  []time.Time // each sweep's Discover
}

var errNoMasters = errors.New("no 1-Wire bus masters")

// read is probe rom's scripted reading in sweep i.
func (s *scenarioSource) read(rom string, i int) reading.Reading {
	if f, ok := s.sc.probes[rom]; ok {
		return f(i)
	}
	return rt.OK(61375)
}

// topology is the bus in sweep i: every probe not scripted as not_present.
func (s *scenarioSource) topology(i int) reading.Topology {
	var on []string
	for _, rom := range s.roms {
		if s.read(rom, i).Code != reading.NotPresent {
			on = append(on, rom)
		}
	}
	return reading.NewTopology(map[string][]string{"w1_bus_master1": on})
}

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
		return s.topology(0), nil
	}
	i := len(s.starts)
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	if d, ok := s.sc.wall[i]; ok {
		s.clock.step(d)
	}
	if s.sc.sigterm != nil && *s.sc.sigterm == i {
		s.sig <- syscall.SIGTERM
	}
	if s.sc.discover[i] {
		return reading.Topology{}, errNoMasters
	}
	return s.topology(i), nil
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
		default:
			r = s.read(rom, i)
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
