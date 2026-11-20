package metertest

import (
	"sync"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// Fake is a meter.Source a test drives: Push an edge at a level (numbered
// on from the last), or Send one exactly as given, gaps and all. Its
// channel holds 1024 edges, so a test can push ahead of its reader.
type Fake struct {
	initial meter.Level
	edges   chan meter.Edge

	mu     sync.Mutex
	seq    uint64
	closed bool
}

// NewFake is a requested line at the initial level.
func NewFake(initial meter.Level) *Fake {
	return &Fake{initial: initial, edges: make(chan meter.Edge, 1024)}
}

// Push sends an edge to l at at, with the next seqno.
func (f *Fake) Push(at time.Duration, l meter.Level) {
	f.mu.Lock()
	seq := f.seq + 1
	f.mu.Unlock()
	f.Send(meter.Edge{At: at, Level: l, Seqno: seq})
}

// Send sends e as given. After Close it is dropped, as a released line
// reports nothing.
func (f *Fake) Send(e meter.Edge) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.seq = e.Seqno
	f.edges <- e
}

// Initial is the level NewFake was given.
func (f *Fake) Initial() meter.Level { return f.initial }

// Edges delivers what was pushed or sent, until Close.
func (f *Fake) Edges() <-chan meter.Edge { return f.edges }

// Close closes Edges; a second Close is a no-op.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.edges)
	}
	return nil
}
