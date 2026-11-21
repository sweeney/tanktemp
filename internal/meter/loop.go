package meter

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Loop owns a Counter: Run is the only goroutine that touches it. It takes
// each edge from the Source, each tick, and each Snapshot request in turn,
// so the Counter needs no lock, and a held closing is confirmed at the
// first edge or tick after its hold. Ticks carry "now" on the same
// monotonic clock as the edges; main sends one every 50 ms (GUESSES G59),
// which bounds a pulse's publish to hold + 50 ms after its closing.
//
// Before a tick or a snapshot, Run takes every edge already queued (for a
// snapshot, every one stamped at or before its now), so neither can
// overtake an earlier edge. An edge still inside the kernel or
// go-gpiocdev's watcher when a tick is read can't be taken: so a closing
// whose reopening is in flight then, and that is shorter than the hold by
// no more than the delivery latency (microseconds to a millisecond or
// two), counts. That is harmless: a real closing is at least about 150 ms.
type Loop struct {
	k     *Counter
	src   Source
	ticks <-chan time.Duration
	emit  func(Event)
	reqs  chan request
	done  chan struct{}
	held  *Edge // taken from the Source but stamped after a request's now: next

	// Trace, if set before Run, is called on Run's goroutine after each
	// edge and each tick (or request's now) with what the counter made of
	// it: pulsewatch's per-edge verdicts, from this same loop.
	Trace func(Step)
}

// Step is one thing the loop handled: an edge (Edge non-nil) or the passage
// of time to Now; the counter's state before and after, and what it
// emitted.
type Step struct {
	Edge          *Edge
	Now           time.Duration
	Before, After Snapshot
	Events        []Event
}

func (l *Loop) observe(e Edge) {
	var before Snapshot
	if l.Trace != nil {
		before = l.k.Snapshot()
	}
	evs := l.k.Observe(e)
	l.publish(evs)
	if l.Trace != nil {
		l.Trace(Step{Edge: &e, Now: e.At, Before: before, After: l.k.Snapshot(), Events: evs})
	}
}

func (l *Loop) tick(now time.Duration) {
	var before Snapshot
	if l.Trace != nil {
		before = l.k.Snapshot()
	}
	evs := l.k.Tick(now)
	l.publish(evs)
	if l.Trace != nil {
		l.Trace(Step{Now: now, Before: before, After: l.k.Snapshot(), Events: evs})
	}
}

// NewLoop is a loop over k and src, calling emit (on Run's goroutine) for
// each event, in order. emit should not block for long: edges queue in the
// Source meanwhile, and past its buffer they are lost.
func NewLoop(k *Counter, src Source, ticks <-chan time.Duration, emit func(Event)) *Loop {
	return &Loop{k: k, src: src, ticks: ticks, emit: emit, reqs: make(chan request), done: make(chan struct{})}
}

// ErrSourceClosed is Run's error when the Source's edges stop. If the
// Source can say why (an Err() error method, as gpioedge's line has), Run's
// error wraps it with the reason.
var ErrSourceClosed = errors.New("meter: the line's edges stopped")

func (l *Loop) closed() error {
	if e, ok := l.src.(interface{ Err() error }); ok && e.Err() != nil {
		return fmt.Errorf("%w: %v", ErrSourceClosed, e.Err())
	}
	return ErrSourceClosed
}

// Run runs until ctx ends (nil) or the Source closes (ErrSourceClosed).
func (l *Loop) Run(ctx context.Context) error {
	defer close(l.done)
	for {
		if e := l.held; e != nil {
			l.held = nil
			l.observe(*e)
		}
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-l.src.Edges():
			if !ok {
				return l.closed()
			}
			l.observe(e)
		case now := <-l.ticks:
			// Every edge already queued first: select picks at random
			// among ready cases, and a tick must never confirm a closing
			// whose (earlier) reopening is waiting behind it.
			if !l.drain(maxDuration) {
				return l.closed()
			}
			l.tick(now)
		case r := <-l.reqs:
			// Likewise up to its now, then confirm what now confirms: "as
			// of now". An edge stamped later waits (in held) until after.
			if !l.drain(r.now) {
				close(r.reply)
				return l.closed()
			}
			l.tick(r.now)
			s := l.k.Snapshot()
			if r.sweep {
				l.emit(Event{Kind: Swept, At: r.now, Snap: s})
			}
			r.reply <- s
		}
	}
}

const maxDuration = time.Duration(1<<63 - 1)

// drain takes the edges already queued, up to the first stamped after upTo
// (kept in held); false if the Source closed.
func (l *Loop) drain(upTo time.Duration) bool {
	for {
		if e := l.held; e != nil {
			if e.At > upTo {
				return true
			}
			l.held = nil
			l.observe(*e)
		}
		select {
		case e, ok := <-l.src.Edges():
			if !ok {
				return false
			}
			if e.At > upTo {
				l.held = &e
				return true
			}
			l.observe(e)
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

type request struct {
	now   time.Duration
	sweep bool
	reply chan Snapshot
}

// Snapshot is the counter's state as of now (monotonic, the edges' clock):
// after every edge the Source has delivered so far that is stamped at or
// before now, and with any change whose hold has passed by now confirmed
// (and published) first.
// false if Run has stopped, or ctx ended first.
func (l *Loop) Snapshot(ctx context.Context, now time.Duration) (Snapshot, bool) {
	return l.ask(ctx, request{now: now, reply: make(chan Snapshot, 1)})
}

// Sweep is Snapshot that also emits the state as a Swept event, on Run's
// goroutine, in order with the litres: what a caller publishes in emit
// order is then in the counter's order.
func (l *Loop) Sweep(ctx context.Context, now time.Duration) bool {
	_, ok := l.ask(ctx, request{now: now, sweep: true, reply: make(chan Snapshot, 1)})
	return ok
}

func (l *Loop) ask(ctx context.Context, r request) (Snapshot, bool) {
	select {
	case l.reqs <- r:
		select { // reply is buffered: the loop never waits on an abandoned one
		case s, ok := <-r.reply:
			return s, ok
		case <-ctx.Done():
		}
	case <-l.done:
	case <-ctx.Done():
	}
	return Snapshot{}, false
}
