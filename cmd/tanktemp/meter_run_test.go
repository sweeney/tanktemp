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
	d.SysFS = fstest.MapFS{"proc/loadavg": {Data: []byte("0.1 0.2 0.3 1/1 1\n")}}
	d.Disk = func() (sysinfo.Disk, error) { return sysinfo.Disk{TotalBytes: 1 << 30, FreeBytes: 1 << 29}, nil }
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
		!strings.Contains(out.String(), ", meter hot_water_meter on gpiochip0:27 (1 L/pulse, debounce 20ms, hold 20ms, min interval 600ms), publishing to water/hot/tanktemp on tcp://b:1883\n") {
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
