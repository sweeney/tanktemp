package readingtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
)

const (
	a1   = "28-0000000000a1"
	a2   = "28-0000000000a2"
	b1   = "28-0000000000b1"
	gone = "28-0000000000ff"
)

func twoBuses() map[string][]string {
	return map[string][]string{"w1_bus_master1": {a1, a2}, "w1_bus_master2": {b1}}
}

// The fake must keep the same rules as the real handler, or tests of the
// core against it prove nothing.
func TestFakeKeepsTheContract(t *testing.T) {
	Contract(t, func(t *testing.T) Fixture {
		f := NewFake(twoBuses()).
			Script(a1, OK(17000)).
			Script(a2, OK(17062)).
			Script(b1, OK(16937))
		return Fixture{Source: f, Present: []string{a1, a2, b1}, Absent: gone}
	})
}

func TestFakePlaysScriptsInOrderAndRepeatsTheLast(t *testing.T) {
	f := NewFake(twoBuses()).Script(a1, OK(17000), Fail(reading.CRC), OK(18000))
	ctx := context.Background()
	top, _ := f.Discover(ctx)
	var got []reading.Code
	var vals []int
	for i := 0; i < 4; i++ {
		r := f.Read(ctx, top, []string{a1})[a1]
		got = append(got, r.Code)
		if r.MilliC != nil {
			vals = append(vals, *r.MilliC)
		}
	}
	want := []reading.Code{reading.OK, reading.CRC, reading.OK, reading.OK}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("read %d = %s, want %s", i, got[i], want[i])
		}
	}
	if len(vals) != 3 || vals[2] != 18000 {
		t.Errorf("values = %v, want the last script entry repeated", vals)
	}
}

func TestFakeUnscriptedPresentROMIsAReadError(t *testing.T) {
	// A forgotten script must show up in the test, not read as a quiet 0.
	f := NewFake(twoBuses())
	top, _ := f.Discover(context.Background())
	if r := f.Read(context.Background(), top, []string{a2})[a2]; r.Code != reading.ReadError {
		t.Errorf("unscripted = %+v, want read_error", r)
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	f := NewFake(twoBuses()).Script(a1, OK(1)).Script(b1, OK(2))
	top, _ := f.Discover(context.Background())
	f.Read(context.Background(), top, []string{b1, a1})
	f.Read(context.Background(), top, nil)
	calls := f.Calls()
	if len(calls) != 2 || len(calls[0]) != 2 || calls[0][0] != b1 || len(calls[1]) != 0 {
		t.Errorf("Calls = %v", calls)
	}
}

func TestFakeTopologyCanChange(t *testing.T) {
	f := NewFake(twoBuses()).Script(a1, OK(1))
	f.SetTopology(map[string][]string{"w1_bus_master1": {b1}, "w1_bus_master2": {a2}})
	top, err := f.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := top.Bus(a1); ok {
		t.Error("a1 should have gone")
	}
	if bus, _ := top.Bus(b1); bus != "w1_bus_master1" {
		t.Errorf("b1 bus = %s, want the swapped bus", bus)
	}
	if r := f.Read(context.Background(), top, []string{a1})[a1]; r.Code != reading.NotPresent {
		t.Errorf("a1 = %s, want not_present once it has left the bus", r.Code)
	}
}

func TestFakeDiscoverError(t *testing.T) {
	boom := errors.New("boom")
	f := NewFake(nil)
	f.DiscoverErr = boom
	if _, err := f.Discover(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

func TestFakeLatencyIsPerBusAndParallel(t *testing.T) {
	f := NewFake(twoBuses()).Script(a1, OK(1)).Script(a2, OK(2)).Script(b1, OK(3))
	f.Latency = 40 * time.Millisecond
	top, _ := f.Discover(context.Background())
	start := time.Now()
	got := f.Read(context.Background(), top, []string{a1, a2, b1})
	took := time.Since(start)
	// Two reads on bus 1 in series, one on bus 2 alongside: ~80ms, not 120ms.
	if took < 75*time.Millisecond || took > 115*time.Millisecond {
		t.Errorf("took %v, want ~80ms (buses in parallel, reads on one bus in series)", took)
	}
	if got[a2].Took < 35*time.Millisecond {
		t.Errorf("Took = %v, want the per-read latency", got[a2].Took)
	}
}

func TestFakeCancelMidSweep(t *testing.T) {
	f := NewFake(twoBuses()).Script(a1, OK(1)).Script(a2, OK(2)).Script(b1, OK(3))
	f.Latency = 50 * time.Millisecond
	top, _ := f.Discover(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := f.Read(ctx, top, []string{a1, a2, b1})
	if took := time.Since(start); took > 90*time.Millisecond {
		t.Errorf("took %v after cancel at 70ms", took)
	}
	if got[a1].Code != reading.OK || got[b1].Code != reading.OK {
		t.Errorf("reads finished before the cancel should stand: %+v", got)
	}
	if got[a2].Code != reading.Cancelled {
		t.Errorf("a2 = %s, want cancelled", got[a2].Code)
	}
}

func TestFakeInspect(t *testing.T) {
	f := NewFake(twoBuses())
	f.Infos = map[string]reading.Info{a1: {ExtPower: Bool(true), Resolution: Int(12)}}
	info, err := f.Inspect(context.Background(), a1)
	if err != nil || !*info.ExtPower || *info.Resolution != 12 {
		t.Errorf("Inspect = %+v, %v", info, err)
	}
	if _, err := f.Inspect(context.Background(), gone); !errors.Is(err, ErrNoInfo) {
		t.Errorf("uninspectable ROM: err = %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if r := Suspect(85000); r.Code != reading.OK || r.Suspect != reading.Suspect85 || *r.MilliC != 85000 {
		t.Errorf("Suspect = %+v", r)
	}
	if r := Fail(reading.BusLow); r.Code != reading.BusLow || r.MilliC != nil {
		t.Errorf("Fail = %+v", r)
	}
	if r := Tries(OK(5), 3); r.Attempts != 3 {
		t.Errorf("Tries = %+v", r)
	}
}
