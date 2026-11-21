package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sweeney/common/health/sysinfo"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
	"github.com/sweeney/tanktemp/internal/reading"
	rt "github.com/sweeney/tanktemp/internal/reading/readingtest"
)

const meterTopic = prefix + "/meter"

func meterConfig(t *testing.T) config.Config {
	t.Helper()
	c, err := config.Load([]byte(`
mqtt: {broker: tcp://broker.example:1883, topic_prefix: test/water/hot/tanktemp}
clock_sync_wait: 0s
tanks:
  - device_id: hot-water-cylinder
    sensors:
      - {rom: 28-0000117287e6, position: top, order: 2, height_mm: 1500}
      - {rom: 28-000011721ce9, position: bottom, order: 1, height_mm: 100}
meters:
  - {device_id: hot_water_meter, gpio: 27, litres_per_pulse: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func okSource() *rt.Fake {
	src := rt.NewFake(map[string][]string{"w1_bus_master1": {top}, "w1_bus_master2": {bottom}}).
		Script(top, rt.OK(61375)).Script(bottom, rt.OK(12062))
	src.Infos = map[string]reading.Info{
		top:    {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
		bottom: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
	}
	return src
}

type meterRig struct {
	*rig
	line   *metertest.Fake
	ticks  chan time.Duration
	opened []config.Meter
	url    string // the HTTP server's base
	once   sync.Once
}

// stop sends SIGTERM and waits for run to return (once).
func (r *meterRig) stop(t *testing.T) {
	r.once.Do(func() {
		r.sig <- syscall.SIGTERM
		if err := <-r.done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
}

// startMeterRig runs run with a meter whose line is a Fake (or whose
// request fails with openErr), on a fixed monotonic clock: mono 100 s is
// wall t0.
func startMeterRig(t *testing.T, cfg config.Config, openErr error) *meterRig {
	t.Helper()
	return startMeterRigWith(t, cfg, openErr, nil)
}

// startMeterRigWith is startMeterRig with run's publisher wrapped (nil:
// the rig's Fake as it is).
func startMeterRigWith(t *testing.T, cfg config.Config, openErr error, wrap func(publisher) publisher) *meterRig {
	t.Helper()
	r := &meterRig{
		rig:   &rig{pub: &mqttpubtest.Fake{}, tick: make(chan time.Time), hb: make(chan time.Time), sig: make(chan os.Signal, 1), done: make(chan error, 1), logs: &syncBuf{}},
		line:  metertest.NewFake(meter.Open),
		ticks: make(chan time.Duration),
	}
	d := deps{
		Source: okSource(), Pub: r.pub, Synced: func() bool { return true },
		Now: func() time.Time { return t0 }, Tick: r.tick, HeartbeatTick: r.hb, Signals: r.sig,
		Logger: log.New(r.logs, "", 0), Version: "abc1234",
		OpenMeter: func(m config.Meter) (meter.Source, error) {
			r.opened = append(r.opened, m)
			if openErr != nil {
				return nil, openErr
			}
			return r.line, nil
		},
		Mono:       func() time.Duration { return 100 * time.Second },
		MeterTicks: r.ticks,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.Listener, r.url = ln, "http://"+ln.Addr().String()
	d.SysFS = fstest.MapFS{"proc/loadavg": {Data: []byte("0.1 0.2 0.3 1/1 1\n")},
		// One w1-gpio overlay loaded, on GPIO 4 (#10).
		"sys/firmware/devicetree/base/onewire@4/gpios": {Data: []byte{0, 0, 0, 7, 0, 0, 0, 4, 0, 0, 0, 0}}}
	d.Disk = func() (sysinfo.Disk, error) { return sysinfo.Disk{TotalBytes: 1 << 30, FreeBytes: 1 << 29}, nil }
	if wrap != nil {
		d.Pub = wrap(d.Pub)
	}
	go func() { r.done <- run(context.Background(), cfg, d) }()
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *meterRig) meterMsgs(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var got []string
		for _, m := range r.pub.All(meterTopic) {
			got = append(got, m.Payload)
		}
		if len(got) >= n || time.Now().After(deadline) {
			if len(got) < n {
				t.Fatalf("%d meter messages, want %d: %v\nlog: %s", len(got), n, got, r.logs)
			}
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// A configured meter: its line is opened with the config's settings, the
// status names it, every sweep carries a meter message stamped with the
// sweep's time, and a litre is published as it's counted.
func TestRunPublishesTheMeter(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	got := r.meterMsgs(t, 1) // the immediate sweep's
	if !strings.Contains(got[0], `"trigger":"sweep","seq":1`) || !strings.Contains(got[0], `"timestamp":"2026-11-02T21:00:00Z"`) ||
		!strings.Contains(got[0], `"device_id":"hot_water_meter"`) || !strings.Contains(got[0], `"counting_since":"2026-11-02T21:00:00Z"`) {
		t.Errorf("sweep message %s", got[0])
	}
	if len(r.opened) != 1 || r.opened[0].Debounce != 20*time.Millisecond || *r.opened[0].GPIO != 27 {
		t.Errorf("opened %+v", r.opened)
	}
	st, _ := r.pub.Find(prefix + "/status")
	if !strings.Contains(st.Payload, `"meters":["hot_water_meter"]`) {
		t.Errorf("status %s", st.Payload)
	}
	// A litre: closing at mono 99 s (wall t0 - 1 s), confirmed by a tick.
	r.line.Push(99*time.Second, meter.Closed)
	r.ticks <- 99*time.Second + 50*time.Millisecond
	got = r.meterMsgs(t, 2)
	if !strings.Contains(got[1], `"trigger":"pulse"`) || !strings.Contains(got[1], `"count":1`) ||
		!strings.Contains(got[1], `"last_pulse_at":"2026-11-02T20:59:59.000Z"`) {
		t.Errorf("pulse message %s", got[1])
	}
}

// A line that can't be requested is a startup problem (degraded health),
// and the probes carry on without the meter.
func TestRunWithoutItsMeterLine(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), errors.New("GPIO 27 on gpiochip0: permission denied"))
	r.waitFor(t, 3)
	st, _ := r.pub.Find(prefix + "/status")
	if !strings.Contains(st.Payload, `{"rom":null,"code":"gpio_request_failed","message":"meter hot_water_meter: GPIO 27 on gpiochip0: permission denied"}`) ||
		!strings.Contains(st.Payload, `"meters":["hot_water_meter"]`) {
		t.Errorf("status %s", st.Payload)
	}
	if _, ok := r.pub.Find(prefix + "/hot-water-cylinder/state"); !ok {
		t.Error("no tank state without the meter")
	}
	if len(r.pub.All(meterTopic)) != 0 {
		t.Error("meter messages with no line")
	}
	if !strings.Contains(r.logs.String(), "gpio_request_failed") && !strings.Contains(r.logs.String(), "permission denied") {
		t.Errorf("not logged: %s", r.logs)
	}
}

// No meters configured: no line, no meter topic, "meters":[] in status.
func TestRunWithNoMeter(t *testing.T) {
	r := startMeterRig(t, testConfig(t), nil)
	r.waitFor(t, 3)
	st, _ := r.pub.Find(prefix + "/status")
	if len(r.opened) != 0 || len(r.pub.All(meterTopic)) != 0 || !strings.Contains(st.Payload, `"meters":[]`) {
		t.Errorf("opened %v, meter messages %v, status %s", r.opened, r.pub.All(meterTopic), st.Payload)
	}
}

// At SHUTDOWN the meter publishes a last state as of then, after every
// queued litre and before the status says SHUTDOWN (review on 376ae58).
func TestRunFlushesTheMeterAtShutdown(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	r.meterMsgs(t, 1)
	for i := range 3 {
		r.line.Push(time.Duration(90+i)*time.Second, meter.Closed)
		r.line.Push(time.Duration(90+i)*time.Second+400*time.Millisecond, meter.Open)
	}
	r.stop(t)
	msgs := r.pub.Snapshot()
	lastMeter, shutdown := -1, -1
	for i, m := range msgs {
		if m.Topic == meterTopic {
			lastMeter = i
		}
		if m.Topic == prefix+"/status" && strings.Contains(m.Payload, `"event":"SHUTDOWN"`) {
			shutdown = i
		}
	}
	if lastMeter < 0 || shutdown < 0 || lastMeter > shutdown {
		t.Fatalf("last meter message at %d, SHUTDOWN at %d", lastMeter, shutdown)
	}
	if p := msgs[lastMeter].Payload; !strings.Contains(p, `"trigger":"sweep"`) || !strings.Contains(p, `"count":3,`) {
		t.Errorf("last meter message %s", p)
	}
	if n := len(r.pub.All(meterTopic)); n != 1+6+1 {
		t.Errorf("%d meter messages, want 8: the first sweep, 3 pulses and 3 opens, the last state", n)
	}
}

// hookPub calls on with each message once it is published, before
// Publish returns (as a QoS 1 publish waits for its PUBACK).
type hookPub struct {
	publisher
	on func(mqttpub.Message)
}

func (h hookPub) Publish(m mqttpub.Message) error {
	err := h.publisher.Publish(m)
	h.on(m)
	return err
}

// Nothing on the meter topic after SHUTDOWN (review on #9, a blocker): the
// meter used to keep counting until run's deferred stop, so an edge and a
// tick while SHUTDOWN waited for its PUBACK published a retained pulse
// after the "last" state (20 of 20 runs).
func TestRunPublishesNoMeterMessageAfterShutdown(t *testing.T) {
	var r *meterRig
	r = startMeterRigWith(t, meterConfig(t), nil, func(p publisher) publisher {
		return hookPub{p, func(m mqttpub.Message) {
			if m.Topic != prefix+"/status" || !strings.Contains(m.Payload, `"event":"SHUTDOWN"`) {
				return
			}
			r.line.Push(95*time.Second, meter.Closed)
			select {
			case r.ticks <- 96 * time.Second: // well past the hold
			case <-time.After(100 * time.Millisecond): // the loop has stopped: good
			}
			time.Sleep(20 * time.Millisecond)
		}}
	})
	r.meterMsgs(t, 1)
	r.stop(t)
	msgs := r.pub.Snapshot()
	shutdown := -1
	for i, m := range msgs {
		if m.Topic == prefix+"/status" && strings.Contains(m.Payload, `"event":"SHUTDOWN"`) {
			shutdown = i
		}
		if shutdown >= 0 && m.Topic == meterTopic {
			t.Errorf("a meter message after SHUTDOWN: %s", m.Payload)
		}
	}
	if shutdown < 0 {
		t.Fatal("no SHUTDOWN")
	}
}

// A line that stops (the meter isn't counting any more) makes health
// degraded, naming the meter, until restart (review on 376ae58).
func TestRunDegradesWhenTheMeterLineStops(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	r.meterMsgs(t, 1)
	r.line.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(r.url + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "degraded") && strings.Contains(string(b), "meter hot_water_meter stopped") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/healthz %s", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// -check-config shows the meter's settings with the defaults filled in,
// before "publishing to" (deploy/lib.sh reads the prefix and broker from
// the end of the line).
func TestCheckConfigShowsTheMeter(t *testing.T) {
	path := t.TempDir() + "/c.yaml"
	_ = os.WriteFile(path, []byte("mqtt: {broker: tcp://b:1883}\ntanks:\n  - device_id: t\n    sensors:\n      - {rom: 28-0000117287e6, position: top, order: 1}\nmeters:\n  - {device_id: hot_water_meter, gpio: 27, litres_per_pulse: 1}\n"), 0o644)
	var out strings.Builder
	if code := realMain([]string{"-config", path, "-check-config"}, &out); code != 0 ||
		!strings.Contains(out.String(), ", meter hot_water_meter on gpiochip0:27 (pull-up, closed reads low; 1 L/pulse, debounce 20ms, hold 20ms, min interval 600ms), publishing to water/hot/tanktemp on tcp://b:1883\n") {
		t.Errorf("code %d, out %q", code, out.String())
	}
}

// When run returns, the meter's line has been released (Close), as the
// Contract has it.
func TestRunClosesTheMeterLine(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	r.meterMsgs(t, 1)
	r.stop(t)
	select {
	case _, ok := <-r.line.Edges():
		if ok {
			t.Error("an edge after run returned")
		}
	case <-time.After(time.Second):
		t.Error("the line is still open after run returned")
	}
}

// A shutdown is not the line stopping: the meter is stopped before its
// line is released, so no "stopped" fault or log line appears.
func TestRunShutdownIsNotALineStop(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	r.meterMsgs(t, 1)
	r.stop(t)
	if strings.Contains(r.logs.String(), "not counting") {
		t.Errorf("a shutdown logged as the line stopping: %s", r.logs)
	}
}

func (r *meterRig) apiMeters(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(r.url + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	s := strings.Join(strings.Fields(string(b)), "")
	_, m, _ := strings.Cut(s, `"meters":`)
	return m
}

// /api/status (and /healthz) say whether the meter is alive, in each state.
func TestRunReportsTheMeter(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	r.meterMsgs(t, 1)
	r.line.Push(99*time.Second, meter.Closed)
	r.ticks <- 99*time.Second + 50*time.Millisecond
	r.meterMsgs(t, 2)
	if m := r.apiMeters(t); !strings.HasPrefix(m, `[{"device_id":"hot_water_meter","line":"gpiochip0:27","state":"counting","reason":null,"level":"closed","count":1,"counting_since":"2026-11-02T21:00:00Z","last_pulse_at":"2026-11-02T20:59:59.000Z","noise":0,"short":0,"glitches":0,"lost":0,"publish_dropped":0,"queued":0}]`) {
		t.Errorf("counting: %s", m)
	}
	r.line.Close()
	deadline := time.Now().Add(3 * time.Second)
	for m := r.apiMeters(t); !strings.HasPrefix(m, nospace(`[{"device_id":"hot_water_meter","line":"gpiochip0:27","state":"stopped","reason":"meter: the line's edges stopped","level":null,"count":null`)); m = r.apiMeters(t) {
		if time.Now().After(deadline) {
			t.Fatalf("stopped: %s", m)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunReportsAFailedMeter(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), errors.New("GPIO 27 on gpiochip0: device or resource busy"))
	r.waitFor(t, 3)
	if m := r.apiMeters(t); !strings.HasPrefix(m, nospace(`[{"device_id":"hot_water_meter","line":"gpiochip0:27","state":"failed","reason":"GPIO 27 on gpiochip0: device or resource busy","level":null,"count":null,"counting_since":null,"last_pulse_at":null,"noise":null,"short":null,"glitches":null,"lost":null,"publish_dropped":0,"queued":0}]`)) {
		t.Errorf("failed: %s", m)
	}
}

func nospace(s string) string { return strings.Join(strings.Fields(s), "") }

// With the buses declared, a declared one that isn't loaded is a startup
// problem, named (#10); the probes still publish.
func TestRunReportsAMissingBus(t *testing.T) {
	cfg := meterConfig(t)
	four, seventeen := 4, 17
	cfg.OneWire.Buses = []config.Bus{{GPIO: &four}, {GPIO: &seventeen}}
	r := startMeterRig(t, cfg, nil)
	r.waitFor(t, 3)
	st, _ := r.pub.Find(prefix + "/status")
	if !strings.Contains(st.Payload, `{"rom":null,"code":"w1_bus_missing","message":"no 1-Wire bus is loaded on GPIO 17`) ||
		strings.Contains(st.Payload, "GPIO 4,") {
		t.Errorf("status %s", st.Payload)
	}
	if _, ok := r.pub.Find(prefix + "/hot-water-cylinder/state"); !ok {
		t.Error("no tank state")
	}
}

// Without them declared there is no bus check (a config from before #10).
func TestRunWithoutDeclaredBusesChecksNone(t *testing.T) {
	r := startMeterRig(t, meterConfig(t), nil)
	r.waitFor(t, 3)
	if st, _ := r.pub.Find(prefix + "/status"); strings.Contains(st.Payload, "w1_bus") {
		t.Errorf("status %s", st.Payload)
	}
}
