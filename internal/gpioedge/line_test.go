package gpioedge

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
)

// fakeChip is a requested line on no hardware: what a requester returns.
// A test drives it through the handler the line registered, as the
// kernel's events would, and can make its reads fail (the chip gone).
type fakeChip struct {
	mu      sync.Mutex
	value   int
	seq     uint32
	h       func(at time.Duration, high bool, seq uint32)
	broken  error
	closed  bool
	settled Settings
}

func (c *fakeChip) Value() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return 0, c.broken
	}
	return c.value, nil
}

func (c *fakeChip) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("closed twice")
	}
	c.closed = true
	return nil
}

// drive sets the line's raw value and reports the event, as the kernel
// would (unless the line is released).
func (c *fakeChip) drive(v int) {
	c.mu.Lock()
	c.value, c.seq = v, c.seq+1
	h, seq, closed := c.h, c.seq, c.closed
	c.mu.Unlock()
	if !closed {
		h(time.Duration(seq)*time.Second, v != 0, seq)
	}
}

// fakeChips is a requester over one fakeChip per request; it refuses a
// second request while a line is held, as the kernel does.
type fakeChips struct {
	mu   sync.Mutex
	last *fakeChip
	held bool
}

func (f *fakeChips) request(s Settings, h func(time.Duration, bool, uint32)) (chipLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last != nil && !f.last.closed {
		return nil, errors.New("device or resource busy")
	}
	f.last = &fakeChip{value: 1, h: h, settled: s}
	return f.last, nil
}

func (f *fakeChips) chip() *fakeChip { f.mu.Lock(); defer f.mu.Unlock(); return f.last }

var settings = Settings{Chip: "gpiochip0", Offset: 27, Debounce: 20 * time.Millisecond, Bias: PullUp}

// gpioedge's own logic (the request, the initial read, the non-blocking
// hand-off, close ordering, the event mapping) meets the shared Contract
// with no hardware (review on #9), full channel included.
func TestLineContract(t *testing.T) {
	chips := &fakeChips{}
	metertest.Contract(t, metertest.Harness{
		Open: func(t *testing.T) meter.Source {
			t.Helper()
			s, err := open(settings, chips.request, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		Drive: func(t *testing.T, _ meter.Source, levels ...meter.Level) {
			for _, l := range levels {
				v := 0
				if l == meter.Open {
					v = 1
				}
				chips.chip().drive(v)
			}
		},
		Capacity: buffer,
	})
}

// The settings reach the requester as given.
func TestOpenPassesItsSettings(t *testing.T) {
	chips := &fakeChips{}
	want := Settings{Chip: "gpiochip4", Offset: 22, Debounce: 30 * time.Millisecond, Bias: None, ClosedHigh: true}
	s, err := open(want, chips.request, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := chips.chip().settled; got != want {
		t.Errorf("requested %+v, want %+v", got, want)
	}
}

// A line whose reads fail (its chip gone: unbound, ENODEV) is not left
// looking alive (review on #9: go-gpiocdev's watcher swallows read errors,
// so nothing else would notice). The liveness check closes Edges and says
// why, which the meter loop reports.
func TestADeadLineClosesItsEdges(t *testing.T) {
	chips := &fakeChips{}
	s, err := open(settings, chips.request, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := chips.chip()
	c.mu.Lock()
	c.broken = errors.New("no such device")
	c.mu.Unlock()
	if _, ok := metertest.Receive(t, s); ok {
		t.Fatal("an edge from a dead line")
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "no such device") {
		t.Errorf("Err() = %v, want the read's error", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		t.Error("the dead line was not released")
	}
}

// A line whose first read fails is released, and the error names it.
func TestOpenReleasesALineItCannotRead(t *testing.T) {
	broken := &fakeChip{broken: errors.New("no such device")}
	_, err := open(settings, func(Settings, func(time.Duration, bool, uint32)) (chipLine, error) { return broken, nil }, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "GPIO 27 on gpiochip0") {
		t.Errorf("err = %v", err)
	}
	if !broken.closed {
		t.Error("not released")
	}
}

// Which raw level is closed is the wiring's, from the config.
func TestPolarity(t *testing.T) {
	for _, tt := range []struct {
		closedHigh bool
		raw        int
		high       bool
		want       meter.Level
	}{
		{false, 0, false, meter.Closed}, {false, 1, true, meter.Open},
		{true, 0, false, meter.Open}, {true, 1, true, meter.Closed},
	} {
		if got := levelOf(tt.raw, tt.closedHigh); got != tt.want {
			t.Errorf("levelOf(%d, closedHigh %v) = %v, want %v", tt.raw, tt.closedHigh, got, tt.want)
		}
		if e := edgeOf(time.Second, tt.high, 5, tt.closedHigh); e != (meter.Edge{At: time.Second, Level: tt.want, Seqno: 5}) {
			t.Errorf("edgeOf(high %v, closedHigh %v) = %+v, want %v", tt.high, tt.closedHigh, e, tt.want)
		}
	}
}
