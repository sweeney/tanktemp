package meter

import (
	"context"
	"errors"
	"time"
)

// Loop owns a Counter: Run is the only goroutine that touches it. It takes
// each edge from the Source, each tick, and each Snapshot request in turn,
// so the Counter needs no lock, and a held closing is confirmed at the
// first edge or tick after its hold. Ticks carry "now" on the same
// monotonic clock as the edges; main sends one every 50 ms (GUESSES G59),
// which bounds a pulse's publish to hold + 50 ms after its closing.
type Loop struct {
	k     *Counter
	src   Source
	ticks <-chan time.Duration
	emit  func(Event)
	reqs  chan chan Snapshot
	done  chan struct{}
}

// NewLoop is a loop over k and src, calling emit (on Run's goroutine) for
// each event, in order. emit should not block for long: edges queue in the
// Source meanwhile, and past its buffer they are lost.
func NewLoop(k *Counter, src Source, ticks <-chan time.Duration, emit func(Event)) *Loop {
	return &Loop{k: k, src: src, ticks: ticks, emit: emit, reqs: make(chan chan Snapshot), done: make(chan struct{})}
}

// ErrSourceClosed is Run's error when the Source's edges stop.
var ErrSourceClosed = errors.New("meter: the line's edges stopped")

// Run runs until ctx ends (nil) or the Source closes (ErrSourceClosed).
func (l *Loop) Run(ctx context.Context) error {
	defer close(l.done)
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-l.src.Edges():
			if !ok {
				return ErrSourceClosed
			}
			l.publish(l.k.Observe(e))
		case now := <-l.ticks:
			l.publish(l.k.Tick(now))
		case r := <-l.reqs:
			// Take every edge already queued first, so the snapshot is
			// "as of now" and not of whichever case select picked.
			if !l.drain() {
				close(r)
				return ErrSourceClosed
			}
			r <- l.k.Snapshot()
		}
	}
}

// drain takes the edges already queued; false if the Source closed.
func (l *Loop) drain() bool {
	for {
		select {
		case e, ok := <-l.src.Edges():
			if !ok {
				return false
			}
			l.publish(l.k.Observe(e))
		default:
			return true
		}
	}
}

func (l *Loop) publish(evs []Event) {
	for _, e := range evs {
		l.emit(e)
	}
}

// Snapshot is the counter's state after every edge the Source has
// delivered so far.
// false if Run has stopped, or ctx ended first.
func (l *Loop) Snapshot(ctx context.Context) (Snapshot, bool) {
	r := make(chan Snapshot, 1)
	select {
	case l.reqs <- r:
		s, ok := <-r
		return s, ok
	case <-l.done:
	case <-ctx.Done():
	}
	return Snapshot{}, false
}
