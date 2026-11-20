// Package gpioedge is the water meter's GPIO line: the only code that knows
// about the kernel's GPIO character device (via go-gpiocdev, on Linux).
//
// It requests one line as an input with the pull-up on, for both edges,
// with the kernel's debounce and timestamps on the monotonic clock. It reads
// the line's level at the request, then forwards each kernel event as a
// meter.Edge stamped with the kernel's own timestamp and line seqno.
//
// What a kernel event means (drivers/gpio/gpiolib-cdev.c, read 2026-11-20):
//
//   - Its rising/falling id is the line's LEVEL as the kernel read it, not
//     the direction of the edge that interrupted: line_event_id(level). That
//     is why the rig's raw microsecond spikes carried the level the line
//     already had, and why meter.Counter judges levels, never labels.
//   - With debounce (software debounce, used when the pin controller has
//     none, as BCM2835 is believed not to; the rig run will confirm it), an
//     event is emitted only when the level after the line has been quiet
//     for the period differs from the last one, and it is timestamped
//     THEN: about meter.debounce (plus workqueue delay) after the edge.
//     Intervals and closed times are unaffected bar jitter; every time is
//     that much late.
//   - The line seqno counts emitted events. When the kernel's buffer is
//     full it drops the oldest event, whose seqno is never reused, so a
//     loss shows as a gap.
//
// An edge in the moment between the request and reading the initial level
// is already in that level, and its queued event then reads as noise: if it
// was a closing, that litre goes uncounted. That window is microseconds
// wide, once per start.
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

// edgeOf is a kernel event as a meter.Edge. "Rising" is the kernel's level
// read for the event (high), not the edge's direction; high is open.
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
