package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/payload"
	"github.com/sweeney/tanktemp/internal/reading"
	rt "github.com/sweeney/tanktemp/internal/reading/readingtest"
)

const (
	top    = "28-0000117287e6"
	bottom = "28-000011721ce9"
	prefix = "test/energy/hot_water"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load([]byte(`
mqtt: {broker: tcp://broker.example:1883, topic_prefix: test/energy/hot_water}
clock_sync_wait: 0s
tanks:
  - device_id: hot-water-cylinder
    sensors:
      - {rom: 28-0000117287e6, position: top, order: 2, height_mm: 1500, offset_c: -0.19}
      - {rom: 28-000011721ce9, position: bottom, order: 1, height_mm: 100}
`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type rig struct {
	pub   *mqttpubtest.Fake
	src   *rt.Fake
	tick  chan time.Time
	hb    chan time.Time
	sig   chan os.Signal
	done  chan error
	clock time.Time
	logs  *syncBuf
}

// syncBuf is a log destination the test can read while run writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var t0 = time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)

func startRig(t *testing.T, src *rt.Fake) *rig {
	t.Helper()
	return startRigWith(t, src, testConfig(t))
}

// startRigWith runs with cfg, so a test can turn log.verbose on.
func startRigWith(t *testing.T, src *rt.Fake, cfg config.Config) *rig {
	t.Helper()
	r := &rig{
		pub:  &mqttpubtest.Fake{},
		src:  src,
		tick: make(chan time.Time),
		hb:   make(chan time.Time),
		sig:  make(chan os.Signal, 1),
		done: make(chan error, 1),
		logs: &syncBuf{},
	}
	d := deps{
		Source:        src,
		Pub:           r.pub,
		Synced:        func() bool { return true },
		Now:           func() time.Time { return t0 },
		Tick:          r.tick,
		HeartbeatTick: r.hb,
		Signals:       r.sig,
		Logger:        log.New(r.logs, "", 0),
		Version:       "abc1234",
	}
	go func() { r.done <- run(context.Background(), cfg, d) }()
	return r
}

func (r *rig) waitFor(t *testing.T, n int) []mqttpub.Message {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m := r.pub.Snapshot(); len(m) >= n {
			return m
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("got %d messages, want %d: %v", len(r.pub.Snapshot()), n, r.pub.Snapshot())
	return nil
}

func event(t *testing.T, m mqttpub.Message) string {
	t.Helper()
	var s struct{ Event string }
	if err := json.Unmarshal([]byte(m.Payload), &s); err != nil {
		t.Fatalf("%s: %v", m.Payload, err)
	}
	return s.Event
}

func TestLifecycleWiring(t *testing.T) {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}, "w1_bus_master2": {bottom}}).
		Script(top, rt.OK(61375)).
		Script(bottom, rt.OK(12062))
	src.Infos = map[string]reading.Info{
		top:    {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
		bottom: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
	}
	r := startRig(t, src)

	// STARTUP, then (Immediate) the first sweep's state, then the ready
	// heartbeat.
	msgs := r.waitFor(t, 3)
	statusTopic, stateTopic := prefix+"/status", prefix+"/hot-water-cylinder/state"
	if msgs[0].Topic != statusTopic || event(t, msgs[0]) != "STARTUP" || !msgs[0].Retained || msgs[0].QOS != 1 {
		t.Errorf("first = %+v", msgs[0])
	}
	if msgs[1].Topic != stateTopic || !msgs[1].Retained || msgs[1].QOS != 0 {
		t.Errorf("second = %+v", msgs[1])
	}
	var st struct {
		Sensors []struct {
			Position string   `json:"position"`
			TempC    *float64 `json:"temp_c"`
		} `json:"sensors"`
	}
	_ = json.Unmarshal([]byte(msgs[1].Payload), &st)
	// Bottom to top: order 1 is the bottom.
	if len(st.Sensors) != 2 || st.Sensors[0].Position != "bottom" || st.Sensors[1].Position != "top" || *st.Sensors[1].TempC != 61.185 {
		t.Errorf("state = %s", msgs[1].Payload)
	}
	if msgs[2].Topic != statusTopic || event(t, msgs[2]) != "HEARTBEAT" || !strings.Contains(msgs[2].Payload, `"ready":true`) {
		t.Errorf("third = %+v", msgs[2])
	}

	// A scheduled heartbeat, then a tick's sweep.
	r.hb <- t0
	r.waitFor(t, 4)
	r.tick <- t0
	msgs = r.waitFor(t, 5)
	if event(t, msgs[3]) != "HEARTBEAT" || msgs[4].Topic != stateTopic {
		t.Errorf("then = %+v / %+v", msgs[3], msgs[4])
	}

	r.sig <- syscall.SIGTERM
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	msgs = r.pub.Snapshot()
	last := msgs[len(msgs)-1]
	if last.Topic != statusTopic || event(t, last) != "SHUTDOWN" || !strings.Contains(last.Payload, `"reason":"SIGTERM"`) ||
		!strings.Contains(last.Payload, `"online":false`) {
		t.Errorf("last = %+v", last)
	}
}

func TestShutdownMidSweepIsPromptAndPublishesNoPartialSweep(t *testing.T) {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top, bottom}}).
		Script(top, rt.OK(61375)).Script(bottom, rt.OK(12062))
	src.Infos = map[string]reading.Info{top: {}, bottom: {}}
	src.Latency = 300 * time.Millisecond // two sensors on one bus: a 600 ms sweep
	r := startRig(t, src)
	r.waitFor(t, 1) // STARTUP; the immediate sweep is now running
	time.Sleep(50 * time.Millisecond)
	begin := time.Now()
	r.sig <- syscall.SIGTERM
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > 400*time.Millisecond {
		t.Errorf("shutdown took %v: it should stop within one read", took)
	}
	for _, m := range r.pub.Snapshot() {
		if strings.HasSuffix(m.Topic, "/state") {
			t.Errorf("published a cancelled sweep: %s", m.Payload)
		}
	}
	if msgs := r.pub.Snapshot(); event(t, msgs[len(msgs)-1]) != "SHUTDOWN" {
		t.Errorf("last = %+v", msgs[len(msgs)-1])
	}
}

func TestStartupProblemsAreInTheStartupMessage(t *testing.T) {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}}). // bottom missing
										Script(top, rt.OK(1))
	src.Infos = map[string]reading.Info{top: {ExtPower: rt.Bool(false)}}
	r := startRig(t, src)
	msgs := r.waitFor(t, 1)
	if !strings.Contains(msgs[0].Payload, `"code":"parasite_power"`) || !strings.Contains(msgs[0].Payload, `"code":"not_present"`) {
		t.Errorf("startup = %s", msgs[0].Payload)
	}
	r.sig <- syscall.SIGTERM
	<-r.done
}

func TestSweepLogNamesTheFaults(t *testing.T) {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}}). // bottom unplugged
										Script(top, rt.OK(61375))
	src.Infos = map[string]reading.Info{top: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)}}
	r := startRig(t, src)
	r.waitFor(t, 2) // STARTUP and the first state
	r.sig <- syscall.SIGTERM
	<-r.done
	want := "sweep: 1 ok, 1 failed, 0s: not_present: hot-water-cylinder/bottom\n"
	if !strings.Contains(r.logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, r.logs.String())
	}
}

func TestEmitMapsEveryKind(t *testing.T) {
	// The lifecycle's Ready is published as a HEARTBEAT, as in WIRE.md.
	if got := eventName(0); got != payload.EventStartup {
		t.Errorf("Startup = %s", got)
	}
}

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(good, []byte("mqtt: {broker: tcp://b:1883}\ntanks:\n  - device_id: t\n    sensors:\n      - {rom: 28-0000117287e6, position: top, order: 1}\n"), 0o644)
	_ = os.WriteFile(bad, []byte("tanks: []\n"), 0o644)
	var out strings.Builder
	// deploy/lib.sh's parse_check_config reads the prefix and broker from
	// the end of this line, so it must still end with them.
	if code := realMain([]string{"-config", good, "-check-config"}, &out); code != 0 || !strings.Contains(out.String(), ": ok: 1 tank, 1 sensor, ") ||
		!strings.HasSuffix(out.String(), ", publishing to energy/hot_water on tcp://b:1883\n") {
		t.Errorf("good: code %d, out %q", code, out.String())
	}
	out.Reset()
	if code := realMain([]string{"-config", bad, "-check-config"}, &out); code != 1 || !strings.Contains(out.String(), "no tanks") {
		t.Errorf("bad: code %d, out %q", code, out.String())
	}
	out.Reset()
	if code := realMain([]string{"-config", filepath.Join(dir, "missing.yaml"), "-check-config"}, &out); code != 1 {
		t.Errorf("missing: code %d", code)
	}
	out.Reset()
	if code := realMain([]string{"-version"}, &out); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Errorf("-version: %d %q", code, out.String())
	}
}

// verboseConfig is testConfig with log.verbose on.
func verboseConfig(t *testing.T) config.Config {
	t.Helper()
	c := testConfig(t)
	c.Log.Verbose = true
	return c
}

// sweepLines returns the log lines that report a sweep: the verbose line
// ("sweep 4.0s …") and the fault line ("sweep: …").
func sweepLines(logs string) []string {
	var out []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.HasPrefix(l, "sweep") {
			out = append(out, l)
		}
	}
	return out
}

// cleanSource is both probes reading well, on two buses.
func cleanSource() *rt.Fake {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}, "w1_bus_master2": {bottom}}).
		Script(top, rt.OK(61375)).
		Script(bottom, rt.OK(12062))
	src.Infos = map[string]reading.Info{
		top:    {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
		bottom: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
	}
	return src
}

// Issue #2, normal running: a clean sweep says nothing at all.
func TestVerboseOffCleanSweepLogsNothing(t *testing.T) {
	r := startRig(t, cleanSource())
	r.waitFor(t, 3) // STARTUP, the first state, the ready heartbeat
	r.tick <- t0
	r.waitFor(t, 4) // a second sweep
	r.sig <- syscall.SIGTERM
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	if got := sweepLines(r.logs.String()); len(got) != 0 {
		t.Errorf("verbose off, clean sweeps logged %q", got)
	}
}

// Issue #2, at the bench: one line per sweep, every probe bottom to top.
func TestVerboseOnLogsEverySweep(t *testing.T) {
	r := startRigWith(t, cleanSource(), verboseConfig(t))
	r.waitFor(t, 3)
	r.tick <- t0
	r.waitFor(t, 4)
	r.sig <- syscall.SIGTERM
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	// The fake clock doesn't move, so every sweep takes 0.0s. bottom is
	// 12.0625, top is 61.375 - 0.19.
	want := "sweep 0.0s 2/2 hot-water-cylinder 12.1 61.2"
	got := sweepLines(r.logs.String())
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Errorf("verbose on: sweep lines = %q, want two of %q", got, want)
	}
}

// A failed probe is null in its place, and the fault line is still there,
// after the verbose line.
func TestVerboseOnFaultySweepShowsNullAndTheFault(t *testing.T) {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}}). // bottom unplugged
										Script(top, rt.OK(61375))
	src.Infos = map[string]reading.Info{top: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)}}
	r := startRigWith(t, src, verboseConfig(t))
	r.waitFor(t, 2)
	r.sig <- syscall.SIGTERM
	<-r.done
	want := []string{
		"sweep 0.0s 1/2 hot-water-cylinder null 61.2",
		"sweep: 1 ok, 1 failed, 0s: not_present: hot-water-cylinder/bottom",
	}
	if got := sweepLines(r.logs.String()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sweep lines = %q\nwant %q", got, want)
	}
}

// Verbose must not change behaviour: the same sweeps publish the same
// messages either way.
func TestVerboseDoesNotChangeWhatIsPublished(t *testing.T) {
	publish := func(cfg config.Config) []mqttpub.Message {
		r := startRigWith(t, cleanSource(), cfg)
		r.waitFor(t, 3)
		r.tick <- t0
		r.waitFor(t, 4)
		r.sig <- syscall.SIGTERM
		<-r.done
		return r.pub.Snapshot()
	}
	quiet, verbose := publish(testConfig(t)), publish(verboseConfig(t))
	if len(quiet) != len(verbose) {
		t.Fatalf("published %d messages quiet, %d verbose", len(quiet), len(verbose))
	}
	for i := range quiet {
		if quiet[i] != verbose[i] {
			t.Errorf("message %d differs:\nquiet   %+v\nverbose %+v", i, quiet[i], verbose[i])
		}
	}
}

// -check-config shows the verbose switch, so it's seen before a deploy as
// well as in /api/status after one (issue #2).
func TestCheckConfigShowsVerbose(t *testing.T) {
	dir := t.TempDir()
	base := "mqtt: {broker: tcp://b:1883}\ntanks:\n  - device_id: t\n    sensors:\n      - {rom: 28-0000117287e6, position: top, order: 1}\n"
	for _, tt := range []struct{ extra, want string }{
		{"", "verbose log off"},
		{"log: {verbose: false}\n", "verbose log off"},
		{"log: {verbose: true}\n", "verbose log ON"},
	} {
		path := filepath.Join(dir, "c.yaml")
		_ = os.WriteFile(path, []byte(base+tt.extra), 0o644)
		var out strings.Builder
		if code := realMain([]string{"-config", path, "-check-config"}, &out); code != 0 || !strings.Contains(out.String(), tt.want) {
			t.Errorf("%q: code %d, out %q, want it to say %q", tt.extra, code, out.String(), tt.want)
		}
	}
}
