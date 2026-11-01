package sweep

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
	top    = "28-0000000000a1"
	mid    = "28-0000000000a2"
	bottom = "28-0000000000b1"
	other  = "28-0000000000c1" // second tank
	stray  = "28-0000000000ee" // on a bus, not configured
)

func h(mm int) *int { return &mm }

func tanks() []config.Tank {
	return []config.Tank{
		{DeviceID: "cylinder", Sensors: []config.Sensor{
			{ROM: top, Position: "top", Order: 1, HeightMM: h(1500), OffsetC: -0.19},
			{ROM: mid, Position: "middle", Order: 2},
			{ROM: bottom, Position: "bottom", Order: 3, HeightMM: h(100), OffsetC: 0.05},
		}},
		{DeviceID: "second", Sensors: []config.Sensor{
			{ROM: other, Position: "top", Order: 1},
		}},
	}
}

func buses() map[string][]string {
	return map[string][]string{
		"w1_bus_master1": {top, mid, stray},
		"w1_bus_master2": {bottom, other},
	}
}

var t0 = time.Date(2026, 11, 2, 21, 6, 26, 0, time.UTC)

type clock struct{ times []time.Time }

func (c *clock) now() time.Time {
	t := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
	}
	return t
}

func newSweeper(src reading.Source, synced bool) *Sweeper {
	c := &clock{times: []time.Time{t0, t0.Add(3988 * time.Millisecond)}}
	return &Sweeper{Tanks: tanks(), Source: src, Now: c.now, Synced: func() bool { return synced }}
}

func allGood() *rt.Fake {
	return rt.NewFake(buses()).
		Script(top, rt.OK(61375)).
		Script(mid, rt.OK(45000)).
		Script(bottom, rt.OK(12062)).
		Script(other, rt.OK(50000))
}

func TestSweepCalibratesAndOrders(t *testing.T) {
	sw, err := newSweeper(allGood(), true).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sw.At.Equal(t0) || sw.Duration != 3988*time.Millisecond || !sw.ClockSynced {
		t.Errorf("At %v Duration %v synced %v", sw.At, sw.Duration, sw.ClockSynced)
	}
	if len(sw.Tanks) != 2 || sw.Tanks[0].DeviceID != "cylinder" || sw.Tanks[1].DeviceID != "second" {
		t.Fatalf("tanks = %+v", sw.Tanks)
	}
	s := sw.Tanks[0].Sensors
	if len(s) != 3 || s[0].Position != "top" || s[1].Position != "middle" || s[2].Position != "bottom" {
		t.Fatalf("sensors out of order: %+v", s)
	}
	// 61.375 - 0.19 = 61.185, without float noise.
	if s[0].TempC == nil || *s[0].TempC != 61.185 || *s[0].RawC != 61.375 || s[0].OffsetC != -0.19 {
		t.Errorf("top = %+v", s[0])
	}
	// The Source's 12062 millidegrees is the kernel's truncation of the
	// register's exact 12.0625 (raw 193/16); raw_c publishes the exact value.
	if *s[2].TempC != 12.1125 || *s[2].RawC != 12.0625 {
		t.Errorf("bottom = temp %v raw %v", *s[2].TempC, *s[2].RawC)
	}
	if *s[1].TempC != 45 || s[1].OffsetC != 0 {
		t.Errorf("an uncalibrated sensor reads raw: %+v", s[1])
	}
	if s[0].Bus != "w1_bus_master1" || s[2].Bus != "w1_bus_master2" || s[0].ROM != top || *s[0].HeightMM != 1500 || s[0].Order != 1 {
		t.Errorf("metadata: %+v / %+v", s[0], s[2])
	}
	if len(sw.Unexpected) != 1 || sw.Unexpected[0] != stray {
		t.Errorf("unexpected = %v, want the stray ROM", sw.Unexpected)
	}
	if ok, failed := sw.Counts(); ok != 4 || failed != 0 {
		t.Errorf("Counts = %d, %d", ok, failed)
	}
}

func TestSweepAsksForExactlyTheConfiguredROMs(t *testing.T) {
	f := allGood()
	if _, err := newSweeper(f, true).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if len(calls) != 1 || len(calls[0]) != 4 {
		t.Fatalf("calls = %v", calls)
	}
	for _, rom := range calls[0] {
		if rom == stray {
			t.Error("read an unconfigured sensor")
		}
	}
}

func TestEveryConfiguredSensorAppearsEvenWhenFailing(t *testing.T) {
	f := rt.NewFake(map[string][]string{"w1_bus_master1": {top}}). // mid, bottom and other unplugged
									Script(top, rt.Tries(rt.Fail(reading.CRC), 3))
	sw, err := newSweeper(f, true).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.Tanks) != 2 || len(sw.Tanks[0].Sensors) != 3 || len(sw.Tanks[1].Sensors) != 1 {
		t.Fatalf("a failing sensor must never vanish: %+v", sw.Tanks)
	}
	got := sw.Tanks[0].Sensors
	if got[0].Code != reading.CRC || got[0].TempC != nil || got[0].RawC != nil || got[0].Attempts != 3 {
		t.Errorf("crc sensor = %+v", got[0])
	}
	if got[1].Code != reading.NotPresent || got[1].Bus != "" || got[1].TempC != nil {
		t.Errorf("unplugged sensor = %+v", got[1])
	}
	if ok, failed := sw.Counts(); ok != 0 || failed != 4 {
		t.Errorf("Counts = %d, %d", ok, failed)
	}
}

func TestSuspectIsCarried(t *testing.T) {
	f := rt.NewFake(buses()).Script(top, rt.Tries(rt.Suspect(85000), 2)).Script(mid, rt.OK(1)).Script(bottom, rt.OK(1)).Script(other, rt.OK(1))
	sw, _ := newSweeper(f, true).Sweep(context.Background())
	if s := sw.Tanks[0].Sensors[0]; s.Suspect != reading.Suspect85 || *s.RawC != 85 || s.Code != reading.OK {
		t.Errorf("top = %+v", s)
	}
}

func TestClockNotSyncedIsFlagged(t *testing.T) {
	sw, _ := newSweeper(allGood(), false).Sweep(context.Background())
	if sw.ClockSynced {
		t.Error("want clock_synced false")
	}
}

func TestDiscoverFailureStillReportsEverySensor(t *testing.T) {
	f := allGood()
	f.DiscoverErr = errors.New("no 1-Wire bus masters")
	f.SetTopology(nil)
	sw, err := newSweeper(f, true).Sweep(context.Background())
	if err != nil {
		t.Fatalf("a discovery failure is reported in the sweep, not instead of it: %v", err)
	}
	if sw.DiscoverError == "" {
		t.Error("DiscoverError not recorded")
	}
	for _, tank := range sw.Tanks {
		for _, s := range tank.Sensors {
			if s.Code != reading.NotPresent {
				t.Errorf("%s = %s", s.ROM, s.Code)
			}
		}
	}
}

func TestCancelledSweepIsDiscarded(t *testing.T) {
	f := allGood()
	f.Latency = 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := (&Sweeper{Tanks: tanks(), Source: f, Now: time.Now, Synced: func() bool { return true }}).Sweep(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("err = %v, want ErrCancelled", err)
	}
}

func TestRoundingIsStable(t *testing.T) {
	// Every 1/16 °C step plus a two-decimal offset rounds to at most 4
	// decimals: no 61.18499999999.
	for raw := -10000; raw <= 100000; raw += 62 {
		for _, off := range []float64{-0.19, 0.05, 0.33, 0} {
			c := calibrate(raw, off)
			if r := float64(int64(c*1e4+copysign(0.5, c))) / 1e4; r != c {
				t.Fatalf("calibrate(%d, %v) = %v, not 4-decimal clean", raw, off, c)
			}
		}
	}
}

// raw_c is exact (WIRE.md): a Source gives the kernel's millidegrees,
// int(raw)*1000/16 truncated toward zero, and every register value the
// DS18B20 can report (-55 to +125 °C in 1/16 steps) comes back as raw/16.
func TestRawIsExactSixteenths(t *testing.T) {
	for raw := -55 * 16; raw <= 125*16; raw++ {
		kernel := raw * 1000 / 16
		got, want := rawC(kernel), float64(raw)/16
		if got != want {
			t.Fatalf("raw %d: kernel %d mC -> %v, want %v", raw, kernel, got, want)
		}
		if c := calibrate(kernel, 0); c != want {
			t.Fatalf("raw %d: calibrate with no offset = %v, want %v", raw, c, want)
		}
	}
}

func copysign(v, s float64) float64 {
	if s < 0 {
		return -v
	}
	return v
}
