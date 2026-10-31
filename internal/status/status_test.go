package status

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
	rt "github.com/sweeney/tanktemp/internal/reading/readingtest"
	"github.com/sweeney/tanktemp/internal/startup"
	"github.com/sweeney/tanktemp/internal/sweep"
)

var start = time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)

const interval = 30 * time.Second

func newTracker(connected bool) *Tracker {
	return New(Options{
		Start:     start,
		Version:   "abc1234",
		Tanks:     []string{"cyl"},
		Interval:  interval,
		Connected: func() bool { return connected },
	})
}

func okSweep(at time.Time) sweep.Sweep {
	v := 20.0
	return sweep.Sweep{At: at, ClockSynced: true, Tanks: []sweep.Tank{{DeviceID: "cyl", Sensors: []sweep.Sensor{
		{ROM: "28-000000000001", Code: reading.OK, TempC: &v, RawC: &v, Attempts: 1},
	}}}}
}

func TestStartingUntilTheFirstSweep(t *testing.T) {
	tr := newTracker(true)
	h := tr.Snapshot().Health(start.Add(time.Second))
	if h.Status != StatusStarting || len(h.Detail) == 0 {
		t.Errorf("health = %+v", h)
	}
	if s := tr.Snapshot(); s.Ready || s.Last != nil {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestOKAfterAGoodSweep(t *testing.T) {
	tr := newTracker(true)
	tr.RecordSweep(okSweep(start.Add(time.Second)))
	h := tr.Snapshot().Health(start.Add(2 * time.Second))
	if h.Status != StatusOK || len(h.Detail) != 0 {
		t.Errorf("health = %+v", h)
	}
	if !tr.Snapshot().Ready {
		t.Error("ready after the first sweep")
	}
}

func TestDegradedCauses(t *testing.T) {
	failed := okSweep(start)
	failed.Tanks[0].Sensors[0] = sweep.Sensor{ROM: "28-000000000001", Code: reading.BusLow, Attempts: 3}
	unsynced := okSweep(start)
	unsynced.ClockSynced = false
	discover := okSweep(start)
	discover.DiscoverError = "no 1-Wire bus masters"
	tests := []struct {
		name      string
		connected bool
		sw        sweep.Sweep
		problems  []startup.Problem
		now       time.Time
		want      string
	}{
		{"mqtt down", false, okSweep(start), nil, start, "MQTT"},
		{"a sensor failing", true, failed, nil, start, "bus_low"},
		{"stale sweep", true, okSweep(start), nil, start.Add(3*interval + time.Second), "last sweep"},
		{"clock not synced", true, unsynced, nil, start, "clock"},
		{"startup problem", true, okSweep(start), []startup.Problem{{ROM: "28-000000000001", Code: startup.CodeParasitePower, Message: "parasite"}}, start, "parasite"},
		{"discovery failed", true, discover, nil, start, "no 1-Wire bus masters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(tt.connected)
			tr.SetStartup(startup.Report{Problems: tt.problems})
			tr.RecordSweep(tt.sw)
			h := tr.Snapshot().Health(tt.now)
			if h.Status != StatusDegraded || !strings.Contains(strings.Join(h.Detail, "|"), tt.want) {
				t.Errorf("health = %+v, want degraded mentioning %q", h, tt.want)
			}
		})
	}
}

func TestNotStaleJustInside(t *testing.T) {
	tr := newTracker(true)
	tr.RecordSweep(okSweep(start))
	if h := tr.Snapshot().Health(start.Add(3 * interval)); h.Status != StatusOK {
		t.Errorf("at exactly 3 intervals: %+v", h)
	}
}

func TestSuspectIsNotDegraded(t *testing.T) {
	sw := okSweep(start)
	sw.Tanks[0].Sensors[0].Suspect = reading.Suspect85
	tr := newTracker(true)
	tr.RecordSweep(sw)
	if h := tr.Snapshot().Health(start); h.Status != StatusOK {
		t.Errorf("a published, flagged value is not a fault: %+v", h)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	tr := newTracker(true)
	tr.SetStartup(startup.Report{Problems: []startup.Problem{{Code: "x"}}})
	tr.RecordSweep(okSweep(start))
	s := tr.Snapshot()
	s.Problems[0].Code = "changed"
	s.Tanks[0] = "changed"
	if s2 := tr.Snapshot(); s2.Problems[0].Code != "x" || s2.Tanks[0] != "cyl" {
		t.Error("a snapshot shares state with the tracker")
	}
	if s.Version != "abc1234" || !s.Start.Equal(start) || s.Interval != interval || !s.MQTTConnected {
		t.Errorf("snapshot = %+v", s)
	}
}

func TestSkippedTicksAreCounted(t *testing.T) {
	tr := newTracker(true)
	tr.SkippedTick()
	tr.SkippedTick()
	if tr.Snapshot().SkippedTicks != 2 {
		t.Error("skipped ticks")
	}
}

func TestConcurrentUse(t *testing.T) {
	tr := newTracker(true)
	var wg sync.WaitGroup
	for n := 0; n < 50; n++ {
		wg.Add(2)
		go func() { defer wg.Done(); tr.RecordSweep(okSweep(start)) }()
		go func() { defer wg.Done(); _ = tr.Snapshot().Health(start) }()
	}
	wg.Wait()
}

// The two wiring faults the rig actually had (docs/samples), replayed
// through the fake, the sweeper and the tracker. Each row is one sweep: what
// the hardware did, and the health that must follow.
func TestFaultReplays(t *testing.T) {
	const left, middle, right = "28-0000000000a1", "28-0000000000a2", "28-0000000000a3"
	cfg := []config.Tank{{DeviceID: "cyl", Sensors: []config.Sensor{
		{ROM: left, Position: "left", Order: 1},
		{ROM: middle, Position: "middle", Order: 2},
		{ROM: right, Position: "right", Order: 3},
	}}}
	onBus := map[string][]string{"w1_bus_master1": {left, middle, right}}
	gone := map[string][]string{"w1_bus_master1": {}}

	type step struct {
		topology map[string][]string
		left     reading.Reading
		middle   reading.Reading
		right    reading.Reading
		want     string // health status
		failed   int
	}
	ok := func(mc int) reading.Reading { return rt.OK(mc) }
	tests := []struct {
		name  string
		steps []step
	}{
		{
			// fault-power-loss-3v3.csv: 3.3 V lost for one read, then the
			// left sensor's power-on 85 °C, which the Source retries away.
			"power loss", []step{
				{onBus, ok(18625), ok(18562), ok(18750), StatusOK, 0},
				{onBus, rt.Tries(rt.Fail(reading.CRC), 3), rt.Tries(rt.Fail(reading.CRC), 3), rt.Tries(rt.Fail(reading.CRC), 3), StatusDegraded, 3},
				{onBus, rt.Tries(ok(18812), 2), ok(18812), ok(18937), StatusOK, 0},
			},
		},
		{
			// fault-data-line.csv: data line held low (0.000s), then the
			// sensors drop off the bus, then they come back with no reset.
			"data line", []step{
				{onBus, ok(20562), ok(20312), ok(20250), StatusOK, 0},
				{onBus, rt.Tries(rt.Fail(reading.BusLow), 3), rt.Tries(rt.Fail(reading.BusLow), 3), rt.Tries(rt.Fail(reading.BusLow), 3), StatusDegraded, 3},
				{gone, rt.OK(0), rt.OK(0), rt.OK(0), StatusDegraded, 3}, // scripts unused: not on the bus
				{onBus, ok(20500), ok(20250), ok(20187), StatusOK, 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := rt.NewFake(onBus)
			for _, s := range tt.steps {
				f.Script(left, s.left).Script(middle, s.middle).Script(right, s.right)
			}
			tr := newTracker(true)
			now := start
			sw := &sweep.Sweeper{Tanks: cfg, Source: f, Now: func() time.Time { return now }, Synced: func() bool { return true }}
			for i, s := range tt.steps {
				now = start.Add(time.Duration(i) * interval)
				f.SetTopology(s.topology)
				got, err := sw.Sweep(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				tr.RecordSweep(got)
				h := tr.Snapshot().Health(now)
				if _, failed := got.Counts(); h.Status != s.want || failed != s.failed {
					t.Errorf("sweep %d: %s with %d failed, want %s with %d (%v)", i+1, h.Status, failed, s.want, s.failed, h.Detail)
				}
				if len(got.Tanks[0].Sensors) != 3 {
					t.Errorf("sweep %d: a sensor vanished", i+1)
				}
			}
		})
	}
}
