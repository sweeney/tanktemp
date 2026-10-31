package startup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
	rt "github.com/sweeney/tanktemp/internal/reading/readingtest"
)

const (
	a = "28-0000000000a1"
	b = "28-0000000000a2"
	c = "28-0000000000a3"
	d = "28-0000000000a4"
)

func tanks() []config.Tank {
	return []config.Tank{{DeviceID: "cyl", Sensors: []config.Sensor{
		{ROM: a, Position: "top", Order: 1},
		{ROM: b, Position: "upper", Order: 2},
		{ROM: c, Position: "lower", Order: 3},
		{ROM: d, Position: "bottom", Order: 4},
	}}}
}

func TestCheck(t *testing.T) {
	f := rt.NewFake(map[string][]string{"w1_bus_master1": {a, b, c}})
	f.Infos = map[string]reading.Info{
		a: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)},
		b: {ExtPower: rt.Bool(false), Resolution: rt.Int(12)},
		c: {ExtPower: rt.Bool(true), Resolution: rt.Int(11)},
	}
	r := Check(context.Background(), tanks(), f)
	want := []Problem{
		{ROM: b, Code: CodeParasitePower},
		{ROM: c, Code: CodeResolution},
		{ROM: d, Code: CodeNotPresent},
	}
	if len(r.Problems) != len(want) {
		t.Fatalf("problems = %+v", r.Problems)
	}
	for i, w := range want {
		if r.Problems[i].ROM != w.ROM || r.Problems[i].Code != w.Code || r.Problems[i].Message == "" {
			t.Errorf("problem %d = %+v, want %s %s with a message", i, r.Problems[i], w.ROM, w.Code)
		}
	}
	if r.OK() {
		t.Error("OK with problems")
	}
}

func TestCheckClean(t *testing.T) {
	f := rt.NewFake(map[string][]string{"w1_bus_master1": {a}})
	f.Infos = map[string]reading.Info{a: {ExtPower: rt.Bool(true), Resolution: rt.Int(12)}}
	r := Check(context.Background(), []config.Tank{{DeviceID: "x", Sensors: []config.Sensor{{ROM: a, Order: 1}}}}, f)
	if !r.OK() || len(r.Problems) != 0 {
		t.Errorf("problems = %+v", r.Problems)
	}
}

func TestUnknownInfoIsNotAProblem(t *testing.T) {
	// An unreadable ext_power or resolution is unknown, not a fault.
	f := rt.NewFake(map[string][]string{"w1_bus_master1": {a}})
	f.Infos = map[string]reading.Info{a: {}}
	r := Check(context.Background(), []config.Tank{{DeviceID: "x", Sensors: []config.Sensor{{ROM: a, Order: 1}}}}, f)
	if !r.OK() {
		t.Errorf("problems = %+v", r.Problems)
	}
}

func TestInspectFailureIsReported(t *testing.T) {
	f := rt.NewFake(map[string][]string{"w1_bus_master1": {a}}) // no Infos: Inspect fails
	r := Check(context.Background(), []config.Tank{{DeviceID: "x", Sensors: []config.Sensor{{ROM: a, Order: 1}}}}, f)
	if len(r.Problems) != 1 || r.Problems[0].Code != CodeInspectFailed {
		t.Errorf("problems = %+v", r.Problems)
	}
}

func TestDiscoverFailure(t *testing.T) {
	f := rt.NewFake(nil)
	f.DiscoverErr = errors.New("no 1-Wire bus masters")
	r := Check(context.Background(), tanks(), f)
	if len(r.Problems) != 1 || r.Problems[0].Code != CodeDiscoverFailed || r.Problems[0].ROM != "" {
		t.Errorf("one problem for the whole bus, not one per sensor: %+v", r.Problems)
	}
}

func TestWaitForSync(t *testing.T) {
	n := 0
	synced := func() bool { n++; return n >= 3 }
	if !WaitForSync(context.Background(), synced, time.Second, time.Millisecond) {
		t.Error("should have seen the sync")
	}
	start := time.Now()
	if WaitForSync(context.Background(), func() bool { return false }, 30*time.Millisecond, 5*time.Millisecond) {
		t.Error("never synced")
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Error("gave up before the wait")
	}
	if !WaitForSync(context.Background(), func() bool { return true }, 0, time.Hour) {
		t.Error("already synced with no wait")
	}
	if WaitForSync(context.Background(), func() bool { return false }, 0, time.Hour) {
		t.Error("no wait, not synced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WaitForSync(ctx, func() bool { return false }, time.Hour, time.Millisecond) {
		t.Error("cancelled")
	}
}
