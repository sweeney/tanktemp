package readingtest

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
)

// Fixture is a Source under test and what it is known to hold.
type Fixture struct {
	Source  reading.Source
	Present []string // ROMs Discover must report, each of which reads OK
	Absent  string   // a ROM on no bus
}

// Contract runs the rules every reading.Source must keep. newFixture is
// called once per rule, so a Source with state (a replayed sequence) starts
// fresh each time.
func Contract(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()
	ctx := context.Background()

	t.Run("discover reports every present ROM", func(t *testing.T) {
		fx := newFixture(t)
		top, err := fx.Source.Discover(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, rom := range fx.Present {
			if _, ok := top.Bus(rom); !ok {
				t.Errorf("%s missing from Discover", rom)
			}
		}
		if _, ok := top.Bus(fx.Absent); ok {
			t.Errorf("absent %s reported present", fx.Absent)
		}
	})

	t.Run("every requested ROM gets a consistent reading", func(t *testing.T) {
		fx := newFixture(t)
		top, _ := fx.Source.Discover(ctx)
		want := append(append([]string{}, fx.Present...), fx.Absent)
		got := fx.Source.Read(ctx, top, want)
		if len(got) != len(want) {
			t.Errorf("got %d readings for %d ROMs", len(got), len(want))
		}
		for _, rom := range want {
			r, ok := got[rom]
			if !ok {
				t.Errorf("%s: no reading", rom)
				continue
			}
			if r.ROM != rom {
				t.Errorf("reading under %s says ROM %s", rom, r.ROM)
			}
			if err := r.Check(); err != nil {
				t.Error(err)
			}
			if r.Code == reading.Cancelled {
				t.Errorf("%s cancelled without a cancel", rom)
			}
			if r.Took < 0 {
				t.Errorf("%s: negative Took", rom)
			}
		}
		for _, rom := range fx.Present {
			if r := got[rom]; r.Code != reading.OK {
				t.Errorf("present %s = %s, want ok", rom, r.Code)
			} else if bus, _ := top.Bus(rom); r.Bus != bus {
				t.Errorf("%s: Bus = %q, want %q from the topology", rom, r.Bus, bus)
			}
		}
	})

	t.Run("an absent ROM is not_present after one attempt", func(t *testing.T) {
		fx := newFixture(t)
		top, _ := fx.Source.Discover(ctx)
		r := fx.Source.Read(ctx, top, []string{fx.Absent})[fx.Absent]
		if r.Code != reading.NotPresent || r.Attempts != 1 {
			t.Errorf("absent = %s after %d attempts, want not_present after 1", r.Code, r.Attempts)
		}
	})

	t.Run("a ROM left out of the topology is still read", func(t *testing.T) {
		// A sensor the last Discover missed must still report, not vanish.
		fx := newFixture(t)
		rom := fx.Present[0]
		r := fx.Source.Read(ctx, reading.Topology{}, []string{rom})[rom]
		if err := r.Check(); err != nil {
			t.Error(err)
		}
		if r.Code == reading.Cancelled {
			t.Errorf("%s = cancelled", rom)
		}
	})

	t.Run("a cancelled context returns promptly with nothing read", func(t *testing.T) {
		fx := newFixture(t)
		top, _ := fx.Source.Discover(ctx)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		start := time.Now()
		got := fx.Source.Read(cctx, top, fx.Present)
		if took := time.Since(start); took > 200*time.Millisecond {
			t.Errorf("took %v with a cancelled context", took)
		}
		for _, rom := range fx.Present {
			if r := got[rom]; r.Code != reading.Cancelled {
				t.Errorf("%s = %s, want cancelled", rom, r.Code)
			} else if err := r.Check(); err != nil {
				t.Error(err)
			}
		}
	})

	t.Run("an empty request reads nothing", func(t *testing.T) {
		fx := newFixture(t)
		top, _ := fx.Source.Discover(ctx)
		if got := fx.Source.Read(ctx, top, nil); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
}
