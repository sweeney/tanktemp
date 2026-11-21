// Package gpioedge is the water meter's GPIO line: the only code that knows
// about the kernel's GPIO character device (via go-gpiocdev, on Linux).
//
// It requests one line as an input with the configured bias, for both
// edges, with the kernel's debounce and timestamps on the monotonic clock.
// It reads the line's level at the request, then forwards each kernel event
// as a meter.Edge stamped with the kernel's own timestamp and line seqno,
// closed being whichever raw level the config says (closed_level).
//
// Only line_linux.go's request touches go-gpiocdev; everything else here is
// portable, behind chipLine and requester, so the shared metertest.Contract
// runs against this package itself, with a fake chip, anywhere.
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
//
// go-gpiocdev's watcher ignores a failed event read (v0.9.0), so a line
// whose chip goes away would otherwise look alive for ever. A liveness
// check reads the level every few seconds; when that fails, the line is
// released, Edges closes, and Err says why.
package gpioedge

import (
	"fmt"
	"sync"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// MinDebounce matches config.MinMeterDebounce (GUESSES G54), checked again
// here so no caller can request the line undefended.
const MinDebounce = 5 * time.Millisecond

// Bias is the line's internal resistor.
type Bias int

const (
	PullUp   Bias = iota // the default: a reed to ground
	PullDown             // a reed to 3.3 V
	None                 // an external resistor
)

// Settings is what to request: which line, and the wiring's electrical
// facts (all from the config).
type Settings struct {
	Chip       string
	Offset     int
	Debounce   time.Duration
	Bias       Bias
	ClosedHigh bool // the line reads high with the reed closed
}

// chipLine is a requested line: the part of go-gpiocdev's Line used here.
type chipLine interface {
	Value() (int, error)
	Close() error
}

// requester requests a line, calling h with each kernel event: its
// timestamp, whether the level read for it was high, and its line seqno.
type requester func(s Settings, h func(at time.Duration, high bool, seq uint32)) (chipLine, error)

// buffer is the edge channel's capacity.
const buffer = 256

// Liveness is how often a requested line's level is read to check it is
// still there.
const Liveness = 5 * time.Second

func levelOf(v int, closedHigh bool) meter.Level {
	if (v != 0) == closedHigh {
		return meter.Closed
	}
	return meter.Open
}

// edgeOf is a kernel event as a meter.Edge. high is the kernel's level read
// for the event (its "rising" id), not the edge's direction.
func edgeOf(at time.Duration, high bool, lineSeqno uint32, closedHigh bool) meter.Edge {
	l := meter.Open
	if high == closedHigh {
		l = meter.Closed
	}
	return meter.Edge{At: at, Level: l, Seqno: uint64(lineSeqno)}
}

// Line is a requested meter line: a meter.Source, with Err for why its
// edges stopped.
type Line struct {
	s       Settings
	cl      chipLine
	initial meter.Level
	edges   chan meter.Edge
	stop    chan struct{}

	mu     sync.Mutex
	closed bool
	err    error
}

// Open requests the line and returns it as a meter.Source. The error names
// the chip and line; main reports it as gpio_request_failed.
func Open(s Settings) (*Line, error) { return open(s, request, Liveness) }

func open(s Settings, req requester, liveness time.Duration) (*Line, error) {
	name := fmt.Sprintf("GPIO %d on %s", s.Offset, s.Chip)
	if s.Debounce < MinDebounce {
		return nil, fmt.Errorf("%s: debounce %v is under %v", name, s.Debounce, MinDebounce)
	}
	l := &Line{s: s, edges: make(chan meter.Edge, buffer), stop: make(chan struct{})}
	cl, err := req(s, l.handle)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	v, err := cl.Value()
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("%s: reading its level: %w", name, err)
	}
	l.mu.Lock()
	l.cl, l.initial = cl, levelOf(v, s.ClosedHigh)
	l.mu.Unlock()
	go l.watch(liveness)
	return l, nil
}

// handle runs on the requester's event goroutine. It never blocks: a full
// channel drops the edge, which the counter sees as a seqno gap (lost).
func (l *Line) handle(at time.Duration, high bool, seq uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	select {
	case l.edges <- edgeOf(at, high, seq, l.s.ClosedHigh):
	default:
	}
}

// watch reads the level every d until Close; the first failed read ends
// the line.
func (l *Line) watch(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if _, err := l.cl.Value(); err != nil {
				l.end(fmt.Errorf("GPIO %d on %s stopped answering: %w", l.s.Offset, l.s.Chip, err))
				return
			}
		}
	}
}

func (l *Line) Initial() meter.Level     { return l.initial }
func (l *Line) Edges() <-chan meter.Edge { return l.edges }

// Err is why Edges closed: nil after Close, or the failed read.
func (l *Line) Err() error { l.mu.Lock(); defer l.mu.Unlock(); return l.err }

// Close releases the line; a second Close is a no-op.
func (l *Line) Close() error { return l.end(nil) }

func (l *Line) end(why error) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed, l.err = true, why
	close(l.edges)
	close(l.stop)
	l.mu.Unlock()
	return l.cl.Close()
}
