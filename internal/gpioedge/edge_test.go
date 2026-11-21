package gpioedge

import (
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// A kernel event becomes an edge: its own timestamp (never time.Now) and
// its line seqno. (Polarity: TestPolarity.)
func TestEdgeOf(t *testing.T) {
	if e := edgeOf(7*time.Second, true, 41, false); e != (meter.Edge{At: 7 * time.Second, Level: meter.Open, Seqno: 41}) {
		t.Errorf("high: %+v", e)
	}
}

// A debounce too short is refused before anything is requested (GUESSES
// G54), on any platform.
func TestOpenRefusesShortDebounce(t *testing.T) {
	if _, err := Open(Settings{Chip: "gpiochip0", Offset: 27, Debounce: 4 * time.Millisecond}); err == nil {
		t.Error("a 4ms debounce was accepted")
	}
}

// Now reads the clock edges are stamped on, CLOCK_MONOTONIC, so the meter
// can turn an edge's time into wall time. It only ever goes forward.
func TestNowIsMonotonic(t *testing.T) {
	a := Now()
	time.Sleep(2 * time.Millisecond)
	if b := Now(); b <= a || b-a < time.Millisecond || b-a > time.Second {
		t.Errorf("Now %v then %v", a, b)
	}
}
