package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/tanktemp/internal/config"
	rt "github.com/sweeney/tanktemp/internal/reading/readingtest"
)

// fastConfig is testConfig with the issue's sweep.fast block.
func fastConfig(t *testing.T) config.Config {
	t.Helper()
	c := testConfig(t)
	f := config.DefaultFast // 10s, 1.5 °C/min, hold 2m, decay 5m
	c.Sweep.Fast = &f
	return c
}

// drawSource is a cylinder through a hot-water draw: the bottom probe
// reads 40 °C for two sweeps, then 36 °C (a 4 °C fall in 30 s, 8 °C/min)
// and stays there. The top doesn't move.
func drawSource() *rt.Fake {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}, "w1_bus_master2": {bottom}}).
		Script(top, rt.OK(61375)).
		Script(bottom, rt.OK(40000), rt.OK(40000), rt.OK(36000))
	src.Infos = cleanSource().Infos
	return src
}

// runVirtual runs tanktemp with no Tick hook, so its own scheduler drives
// the sweeps, on a virtual clock that stops after limit waits. It returns
// the waits (the gaps between sweeps) once the run has come to rest, with
// the logs. verbose says whether deps.Verbose is the logger (as serve makes
// it with log.verbose on) or nil (off).
func runVirtual(t *testing.T, cfg config.Config, src *rt.Fake, limit int, verbose bool) ([]time.Duration, string) {
	t.Helper()
	clock := newVirtualClock(t0, limit)
	pub := &mqttpubtest.Fake{}
	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var logs syncBuf
	logger := log.New(&logs, "", 0)
	d := deps{
		Source: src, Pub: pub,
		Synced: func() bool { return true }, Now: clock.Now, After: clock.After,
		HeartbeatTick: make(chan time.Time), Signals: sig,
		Logger: logger,
	}
	if verbose {
		d.Verbose = logger
	}
	go func() { done <- run(context.Background(), cfg, d) }()
	// One sweep per wait, plus the immediate one, each publishing a state.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		states := 0
		for _, m := range pub.Snapshot() {
			if strings.HasSuffix(m.Topic, "/state") {
				states++
			}
		}
		if states >= limit+1 && len(clock.Waits()) >= limit {
			break
		}
		time.Sleep(time.Millisecond)
	}
	sig <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waits := clock.Waits()
	if len(waits) < limit {
		t.Fatalf("only %d waits, want %d: %v", len(waits), limit, waits)
	}
	return waits[:limit], logs.String()
}

// The issue's run test: a scripted draw moves the tick spacing to the fast
// interval, holds it, and eases back to slow.
func TestADrawSpeedsTheSweepsUpThenEasesBack(t *testing.T) {
	waits, logs := runVirtual(t, fastConfig(t), drawSource(), 50, true)

	// Sweeps 0 and 1 are quiet: 30 s apart.
	for i := 0; i < 2; i++ {
		if waits[i] != 30*time.Second {
			t.Errorf("gap %d = %v, want 30s before the draw", i, waits[i])
		}
	}
	// Sweep 2 sees the draw: fast for the hold, 12 gaps of 10 s.
	for i := 2; i < 14; i++ {
		if waits[i] != 10*time.Second {
			t.Errorf("gap %d = %v, want 10s in the hold", i, waits[i])
		}
	}
	// Then the decay: rising every gap, never past 30 s, until slow.
	i := 14
	for ; i < len(waits) && waits[i] < 30*time.Second; i++ {
		if waits[i] < waits[i-1] {
			t.Errorf("gap %d = %v fell from %v in the decay", i, waits[i], waits[i-1])
		}
	}
	if i-14 < 15 {
		t.Errorf("only %d gaps in the decay: %v", i-14, waits)
	}
	for ; i < len(waits); i++ {
		if waits[i] != 30*time.Second {
			t.Errorf("gap %d = %v after the decay, want 30s", i, waits[i])
		}
	}
	for _, want := range []string{
		"cadence: fast 10s (hot-water-cylinder/bottom -8.0 °C/min)",
		"cadence: decay 10s",
		"cadence: slow 30s",
	} {
		if !strings.Contains(logs, want+"\n") {
			t.Errorf("verbose log lacks %q:\n%s", want, logs)
		}
	}
}

// Without sweep.fast, the same draw changes nothing: every gap is 30 s,
// and nothing about the cadence is logged.
func TestWithoutFastTheGapIsFixed(t *testing.T) {
	waits, logs := runVirtual(t, testConfig(t), drawSource(), 10, true)
	for i, w := range waits {
		if w != 30*time.Second {
			t.Errorf("gap %d = %v, want 30s with no sweep.fast", i, w)
		}
	}
	if strings.Contains(logs, "cadence:") {
		t.Errorf("logged a cadence change with fast off:\n%s", logs)
	}
}

// With log.verbose off (the default), cadence changes aren't logged, though
// the cadence itself works the same.
func TestCadenceLinesAreVerboseOnly(t *testing.T) {
	waits, logs := runVirtual(t, fastConfig(t), drawSource(), 5, false)
	if waits[2] != 10*time.Second {
		t.Fatalf("gaps %v: the draw didn't go fast, so this test proves nothing", waits)
	}
	if strings.Contains(logs, "cadence:") {
		t.Errorf("verbose off, but logged:\n%s", logs)
	}
}

// run hands the cadence to /api/status: slow and adaptive before any draw.
func TestAPIStatusReportsTheCadence(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := startRigHTTP(t, cleanSource(), fastConfig(t), ln)
	r.waitFor(t, 3)
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Sweep struct {
			Adaptive  bool    `json:"adaptive"`
			Mode      string  `json:"mode"`
			IntervalS float64 `json:"interval_s"`
		} `json:"sweep"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if err != nil || !body.Sweep.Adaptive || body.Sweep.Mode != "slow" || body.Sweep.IntervalS != 30 {
		t.Errorf("/api/status sweep = %+v (err %v)", body.Sweep, err)
	}
	r.sig <- syscall.SIGTERM
	<-r.done
}

// -check-config says what the cadence is, before "verbose log" and
// "publishing to" (deploy/lib.sh reads those from the end of the line).
func TestCheckConfigShowsTheCadence(t *testing.T) {
	dir := t.TempDir()
	base := "mqtt: {broker: tcp://b:1883}\ntanks:\n  - device_id: t\n    sensors:\n      - {rom: 28-0000117287e6, position: top, order: 1}\n"
	for _, tt := range []struct{ extra, want string }{
		{"", ", sweep every 30s, verbose log off, publishing to "},
		{"sweep: {interval: 1m}\n", ", sweep every 1m, verbose log off, "},
		{"sweep: {fast: {}}\n", ", sweep every 30s, 10s while a probe changes 1.5 °C/min (hold 2m, decay 5m), verbose log off, "},
		{"sweep: {fast: {interval: 5s, trigger_c_per_min: 2, hold: 0s, decay: 90s}}\n", ", sweep every 30s, 5s while a probe changes 2 °C/min (hold 0s, decay 1m30s), "},
		{"sweep: {interval: 2h, fast: {hold: 1h, decay: 3h0m}}\n", ", sweep every 2h, 10s while a probe changes 1.5 °C/min (hold 1h, decay 3h), "},
	} {
		path := filepath.Join(dir, "c.yaml")
		_ = os.WriteFile(path, []byte(base+tt.extra), 0o644)
		var out strings.Builder
		if code := realMain([]string{"-config", path, "-check-config"}, &out); code != 0 || !strings.Contains(out.String(), tt.want) {
			t.Errorf("%q: code %d, out %q\nwant it to contain %q", tt.extra, code, out.String(), tt.want)
		}
	}
}
