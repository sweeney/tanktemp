package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/gpioedge"
	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
)

const ms = time.Millisecond

var counting = meter.Config{Hold: 20 * ms, MinInterval: 600 * ms}

// watch prints each edge with what the counter made of it, then a summary.
// One run with every verdict, the reed starting open:
//
//	seq 1  1000 ms closed  held
//	seq 2  1400 ms open    confirms litre 1 (at seq 1); the opening is held
//	seq 3  1500 ms open    noise: already open; confirms the opening: closed 400 ms
//	seq 4  2000 ms closed  held
//	seq 5  2005 ms open    short: reopened in 5 ms, under the 20 ms hold
//	seq 6  2100 ms closed  held (1100 ms after litre 1)
//	seq 7  2500 ms open    litre 2
//	seq 8  2600 ms closed  held; litre 2 ended, closed 400 ms
//	seq 9  2700 ms open    glitch: litre 3 would be 500 ms after litre 2
//	seq 13 3600 ms open    3 edges lost before it
func TestWatch(t *testing.T) {
	src := metertest.NewFake(meter.Open)
	for _, e := range []struct {
		at time.Duration
		l  meter.Level
	}{
		{1000, meter.Closed}, {1400, meter.Open}, {1500, meter.Open},
		{2000, meter.Closed}, {2005, meter.Open},
		{2100, meter.Closed}, {2500, meter.Open},
		{2600, meter.Closed}, {2700, meter.Open},
	} {
		src.Push(e.at*ms, e.l)
	}
	src.Send(meter.Edge{At: 3600 * ms, Level: meter.Open, Seqno: 13})
	src.Close()
	var out strings.Builder
	watch(context.Background(), src, nil, counting, &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := [][]string{
		{"seq 1 ", "closed", "+0.000s", "held"},
		{"seq 2 ", "open", "+0.400s", "litre 1"},
		{"seq 3 ", "open", "noise", "litre 1 ended: closed 400ms"},
		{"seq 4 ", "closed"},
		{"seq 5 ", "open", "short"},
		{"seq 6 ", "closed"},
		{"seq 7 ", "open", "litre 2"},
		{"seq 8 ", "closed", "litre 2 ended: closed 400ms"},
		{"seq 9 ", "open", "glitch"},
		{"seq 13 ", "open", "3 lost before it"},
		{"2 litres", "noise 1", "short 1", "glitches 1", "lost 3"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(want), out.String())
	}
	for i, w := range want {
		for _, s := range w {
			if !strings.Contains(lines[i], s) {
				t.Errorf("line %d %q: no %q", i, lines[i], s)
			}
		}
	}
	if strings.Contains(lines[4], "litre") || strings.Contains(lines[8], "litre 3") {
		t.Errorf("a short or a glitch counted:\n%s", out.String())
	}
}

// A tick confirms a closing that rests on the magnet, and says so.
func TestWatchTickConfirms(t *testing.T) {
	src := metertest.NewFake(meter.Open)
	ticks := make(chan time.Duration)
	var out syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); watch(ctx, src, ticks, counting, &out) }()
	src.Push(1000*ms, meter.Closed)
	ticks <- 1050 * ms
	ticks <- 1060 * ms // the first has been handled
	cancel()
	<-done
	if !strings.Contains(out.String(), "litre 1 (confirmed by a tick, 50ms after its closing)") {
		t.Errorf("out:\n%s", out.String())
	}
}

// gateWriter blocks the first Write that contains block until released: a
// slow ssh terminal.
type gateWriter struct {
	syncBuf
	block   string
	once    sync.Once
	blocked chan struct{}
	release chan struct{}
}

func (g *gateWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), g.block) {
		g.once.Do(func() { close(g.blocked); <-g.release })
	}
	return g.syncBuf.Write(p)
}

// A slow terminal can't make pulsewatch count what tanktemp wouldn't
// (review on #9): with the print of a closing's line blocked, its 5 ms
// reopening and a tick are both ready; the tick must not confirm the
// closing first. pulsewatch runs meter.Loop, which drains queued edges
// before a tick. Over 20 rounds, a broken drain is caught with near
// certainty.
func TestWatchASlowTerminalCountsNoSpike(t *testing.T) {
	for i := range 20 {
		src := metertest.NewFake(meter.Open)
		ticks := make(chan time.Duration)
		out := &gateWriter{block: "seq 1 ", blocked: make(chan struct{}), release: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); watch(ctx, src, ticks, counting, out) }()
		src.Push(1000*ms, meter.Closed)
		<-out.blocked
		src.Push(1005*ms, meter.Open)
		go func() { ticks <- 1050 * ms }()
		time.Sleep(time.Millisecond)
		close(out.release)
		ticks <- 1100 * ms
		src.Close()
		<-done
		cancel()
		if s := out.String(); !strings.Contains(s, "0 litres") || !strings.Contains(s, "short 1") {
			t.Fatalf("round %d: a spike counted:\n%s", i, s)
		}
	}
}

func writeConfig(t *testing.T, meters string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := `mqtt: {broker: tcp://broker.example:1883}
tanks:
  - device_id: hot-water-cylinder
    sensors:
      - {rom: 28-000000000001, position: top, order: 1}
` + meters
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// opens records what run asked gpioedge for, and fails the request so run
// stops there.
type opens struct{ got []gpioedge.Settings }

func (o *opens) open(s gpioedge.Settings) (meter.Source, error) {
	o.got = append(o.got, s)
	return nil, errors.New("not on a Pi")
}

// The bench tool watches the pin the service is configured for, with the
// same timings and wiring (review on #9: -gpio defaulted to 27 in code).
func TestRunTakesTheLineFromTheConfig(t *testing.T) {
	p := writeConfig(t, `meters:
  - {device_id: cold_water_meter, gpio: 22, litres_per_pulse: 1, chip: gpiochip4, debounce: 10ms, bias: none, closed_level: high}
`)
	var o opens
	var out, errOut strings.Builder
	if rc := run([]string{"-config", p}, &out, &errOut, o.open); rc != 1 {
		t.Errorf("rc = %d (%s)", rc, errOut.String())
	}
	want := gpioedge.Settings{Chip: "gpiochip4", Offset: 22, Debounce: 10 * ms, Bias: gpioedge.None, ClosedHigh: true}
	if len(o.got) != 1 || o.got[0] != want {
		t.Errorf("opened %+v, want %+v", o.got, want)
	}
}

// Flags override the config's values, one by one.
func TestRunFlagsOverrideTheConfig(t *testing.T) {
	p := writeConfig(t, "meters:\n  - {device_id: hot_water_meter, gpio: 27, litres_per_pulse: 1}\n")
	var o opens
	var out, errOut strings.Builder
	run([]string{"-config", p, "-gpio", "17", "-debounce", "30ms"}, &out, &errOut, o.open)
	if len(o.got) != 1 || o.got[0].Offset != 17 || o.got[0].Debounce != 30*ms || o.got[0].Chip != "gpiochip0" {
		t.Errorf("opened %+v", o.got)
	}
}

// With no config and no -gpio there is no pin to watch: pulsewatch has no
// built-in one.
func TestRunNeedsAPin(t *testing.T) {
	var o opens
	var out, errOut strings.Builder
	rc := run([]string{"-config", filepath.Join(t.TempDir(), "none.yaml")}, &out, &errOut, o.open)
	if rc != 2 || len(o.got) != 0 || !strings.Contains(errOut.String(), "-gpio") {
		t.Errorf("rc %d, opened %+v, said %q", rc, o.got, errOut.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
