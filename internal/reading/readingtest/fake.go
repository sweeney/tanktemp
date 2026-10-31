// Package readingtest provides a scriptable reading.Source for testing the
// core packages without hardware, and Contract, the rules every Source must
// keep. Contract runs against both the Fake and the real 1-Wire handler, so
// the Fake can't drift from what the hardware does.
package readingtest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
)

// OK is a good reading of milliC raw millidegrees.
func OK(milliC int) reading.Reading {
	return reading.Reading{Code: reading.OK, MilliC: &milliC}
}

// Suspect is a good reading flagged as an unverified 85 °C.
func Suspect(milliC int) reading.Reading {
	r := OK(milliC)
	r.Suspect = reading.Suspect85
	return r
}

// Fail is a failed reading.
func Fail(c reading.Code) reading.Reading { return reading.Reading{Code: c} }

// Tries sets the number of attempts a scripted reading reports.
func Tries(r reading.Reading, n int) reading.Reading {
	r.Attempts = n
	return r
}

// Bool and Int are pointer helpers for reading.Info.
func Bool(b bool) *bool { return &b }
func Int(i int) *int    { return &i }

// ErrNoInfo: Inspect was asked about a ROM with no scripted Info.
var ErrNoInfo = errors.New("readingtest: no info scripted")

// Fake is a scriptable reading.Source. It models the hardware as a topology
// (which ROM is on which bus), per-ROM scripts of readings (one per Read call;
// the last repeats), and a per-read latency. Reads on one bus run in series
// and buses in parallel, as the kernel does, and the context is honoured
// between and during reads.
type Fake struct {
	DiscoverErr error
	Infos       map[string]reading.Info
	Latency     time.Duration

	mu      sync.Mutex
	byBus   map[string][]string
	scripts map[string][]reading.Reading
	calls   [][]string
}

// NewFake returns a Fake whose buses hold byBus (bus name → ROMs).
func NewFake(byBus map[string][]string) *Fake {
	f := &Fake{scripts: map[string][]reading.Reading{}}
	f.SetTopology(byBus)
	return f
}

// SetTopology replaces which ROMs are on which bus, e.g. to model a sensor
// being unplugged or bus numbers swapping after a reboot.
func (f *Fake) SetTopology(byBus map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byBus = map[string][]string{}
	for bus, roms := range byBus {
		f.byBus[bus] = append([]string(nil), roms...)
	}
}

// Script queues readings for rom: each Read call takes the next, and the last
// one repeats. A present ROM with no script reads as read_error, so a
// forgotten script shows up in the test.
func (f *Fake) Script(rom string, rs ...reading.Reading) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[rom] = append(f.scripts[rom], rs...)
	return f
}

// Calls returns the ROMs requested by each Read call, in order.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// Discover implements reading.Source.
func (f *Fake) Discover(ctx context.Context) (reading.Topology, error) {
	if f.DiscoverErr != nil {
		return reading.Topology{}, f.DiscoverErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return reading.NewTopology(f.byBus), nil
}

// Inspect implements reading.Source.
func (f *Fake) Inspect(ctx context.Context, rom string) (reading.Info, error) {
	if info, ok := f.Infos[rom]; ok {
		return info, nil
	}
	return reading.Info{}, ErrNoInfo
}

// Read implements reading.Source.
func (f *Fake) Read(ctx context.Context, t reading.Topology, roms []string) map[string]reading.Reading {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{}, roms...))
	present := reading.NewTopology(f.byBus)
	f.mu.Unlock()

	groups := map[string][]string{}
	for _, rom := range roms {
		bus, _ := t.Bus(rom)
		groups[bus] = append(groups[bus], rom)
	}
	buses := make([]string, 0, len(groups))
	for b := range groups {
		buses = append(buses, b)
	}
	sort.Strings(buses)

	out := make(map[string]reading.Reading, len(roms))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, bus := range buses {
		wg.Add(1)
		go func(bus string, roms []string) {
			defer wg.Done()
			for _, rom := range roms {
				r := f.readOne(ctx, present, rom)
				r.Bus = bus
				mu.Lock()
				out[rom] = r
				mu.Unlock()
			}
		}(bus, groups[bus])
	}
	wg.Wait()
	return out
}

func (f *Fake) readOne(ctx context.Context, present reading.Topology, rom string) reading.Reading {
	cancelled := reading.Reading{ROM: rom, Code: reading.Cancelled}
	if ctx.Err() != nil {
		return cancelled
	}
	start := time.Now()
	if f.Latency > 0 {
		timer := time.NewTimer(f.Latency)
		select {
		case <-ctx.Done():
			timer.Stop()
			return cancelled
		case <-timer.C:
		}
	}
	var r reading.Reading
	if _, ok := present.Bus(rom); !ok {
		r = Fail(reading.NotPresent)
	} else {
		r = f.next(rom)
	}
	r.ROM = rom
	if r.Attempts == 0 {
		r.Attempts = 1
	}
	r.Took = time.Since(start)
	if r.MilliC != nil {
		v := *r.MilliC // never share the script's pointer with the caller
		r.MilliC = &v
	}
	return r
}

func (f *Fake) next(rom string) reading.Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.scripts[rom]
	if len(q) == 0 {
		return Fail(reading.ReadError)
	}
	r := q[0]
	if len(q) > 1 {
		f.scripts[rom] = q[1:]
	}
	return r
}

var _ reading.Source = (*Fake)(nil)
