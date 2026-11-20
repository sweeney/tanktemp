package gpioedge

import (
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// With the pull-up on, the reed open reads high and closed reads low.
func TestLevelOf(t *testing.T) {
	if levelOf(1) != meter.Open || levelOf(0) != meter.Closed {
		t.Errorf("levelOf(1) = %v, levelOf(0) = %v", levelOf(1), levelOf(0))
	}
}

// A kernel event becomes an edge: its own timestamp (never time.Now), its
// line seqno, and a rising edge means the line went high: open.
func TestEdgeOf(t *testing.T) {
	if e := edgeOf(7*time.Second, true, 41); e != (meter.Edge{At: 7 * time.Second, Level: meter.Open, Seqno: 41}) {
		t.Errorf("rising: %+v", e)
	}
	if e := edgeOf(8*time.Second, false, 42); e != (meter.Edge{At: 8 * time.Second, Level: meter.Closed, Seqno: 42}) {
		t.Errorf("falling: %+v", e)
	}
}

// A debounce too short is refused before anything is requested (GUESSES
// G54), on any platform.
func TestOpenRefusesShortDebounce(t *testing.T) {
	if _, err := Open("gpiochip0", 27, 4*time.Millisecond); err == nil {
		t.Error("a 4ms debounce was accepted")
	}
}
