// Package gpioedge is the water meter's GPIO line: the only code that knows
// about the kernel's GPIO character device (via go-gpiocdev, on Linux).
//
// It requests one line as an input with the pull-up on, for both edges,
// with the kernel's debounce (gpiolib-cdev reports only a level that held
// for the whole period) and timestamps on the monotonic clock. It reads the
// line's level at the request, then forwards each kernel event as a
// meter.Edge stamped with the kernel's own timestamp and line seqno.
//
// Should the edge channel ever fill (its reader stalled for 256 edges, a
// few minutes even at the meter's maximum flow), an edge is dropped rather
// than blocking the kernel's reader; the gap in seqnos then shows up as
// "lost" in meter.Counter, which is the honest outcome.
package gpioedge

import (
	"fmt"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// MinDebounce matches config.MinMeterDebounce (GUESSES G54), checked again
// here so no caller can request the line undefended.
const MinDebounce = 5 * time.Millisecond

// With the pull-up on, the reed open reads high and closed reads low.
func levelOf(v int) meter.Level {
	if v != 0 {
		return meter.Open
	}
	return meter.Closed
}

// edgeOf is a kernel event as a meter.Edge: a rising edge means the line
// went high, so the reed opened.
func edgeOf(at time.Duration, rising bool, lineSeqno uint32) meter.Edge {
	l := meter.Closed
	if rising {
		l = meter.Open
	}
	return meter.Edge{At: at, Level: l, Seqno: uint64(lineSeqno)}
}

// Open requests the line and returns it as a meter.Source. The error names
// the chip and line; main reports it as gpio_request_failed.
func Open(chip string, offset int, debounce time.Duration) (meter.Source, error) {
	if debounce < MinDebounce {
		return nil, fmt.Errorf("GPIO %d on %s: debounce %v is under %v", offset, chip, debounce, MinDebounce)
	}
	s, err := request(chip, offset, debounce)
	if err != nil {
		return nil, fmt.Errorf("GPIO %d on %s: %w", offset, chip, err)
	}
	return s, nil
}
