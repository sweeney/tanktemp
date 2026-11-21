// Package metertest holds a fake meter.Source and the contract every
// Source must meet, the real GPIO one (internal/gpioedge) included.
package metertest

import (
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// Harness opens Sources for Contract.
type Harness struct {
	// Open returns a fresh, requested Source; each call may follow a Close.
	Open func(t *testing.T) meter.Source
	// Drive makes the line take each level in turn. Nil when nothing can
	// drive it (a real reed on the rig): the edge checks are then skipped.
	Drive func(t *testing.T, s meter.Source, levels ...meter.Level)
	// Capacity is how many unread edges the Source holds; 0 skips the
	// full-buffer check (it needs Drive too).
	Capacity int
}

// Wait is how long Contract waits for an edge, or for Edges to close.
const Wait = 2 * time.Second

// Quiet is how long an undriven line must stay quiet.
const Quiet = 100 * time.Millisecond

// Receive takes the next edge, failing the test after Wait.
func Receive(t *testing.T, s meter.Source) (meter.Edge, bool) {
	t.Helper()
	select {
	case e, ok := <-s.Edges():
		return e, ok
	case <-time.After(Wait):
		t.Fatalf("no edge, and Edges not closed, after %v", Wait)
		return meter.Edge{}, false
	}
}

// Contract checks a Source: the docs on meter.Source, as tests.
func Contract(t *testing.T, h Harness) {
	t.Run("the initial level is open or closed", func(t *testing.T) {
		s := h.Open(t)
		defer s.Close()
		if l := s.Initial(); l != meter.Open && l != meter.Closed {
			t.Errorf("Initial() = %v", l)
		}
	})
	// Quiet, not open: a real line at rest may read closed (the pointer
	// resting on the magnet).
	t.Run("a line nothing drives stays quiet", func(t *testing.T) {
		s := h.Open(t)
		defer s.Close()
		select {
		case e, ok := <-s.Edges():
			if ok {
				t.Errorf("an edge with nothing driving the line: %+v", e)
			} else {
				t.Error("Edges closed before Close")
			}
		case <-time.After(Quiet):
		}
	})
	t.Run("Close closes Edges, and a second Close is a no-op", func(t *testing.T) {
		s := h.Open(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, ok := Receive(t, s); ok {
			t.Error("Edges still open after Close")
		}
		if err := s.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
	})
	t.Run("Close releases the line: it can be opened again", func(t *testing.T) {
		h.Open(t).Close()
		s := h.Open(t)
		s.Close()
	})
	if h.Drive == nil {
		t.Log("no driver: the edge checks are skipped")
		return
	}
	t.Run("edges come in seqno order from 1, on the monotonic clock, at the levels driven", func(t *testing.T) {
		s := h.Open(t)
		defer s.Close()
		l := s.Initial()
		var want []meter.Level
		for range 4 {
			l = map[meter.Level]meter.Level{meter.Open: meter.Closed, meter.Closed: meter.Open}[l]
			want = append(want, l)
		}
		h.Drive(t, s, want...)
		var prev meter.Edge
		for i, w := range want {
			e, ok := Receive(t, s)
			if !ok {
				t.Fatalf("Edges closed after %d edges", i)
			}
			if e.Level != w {
				t.Errorf("edge %d: level %v, want %v", i, e.Level, w)
			}
			if i == 0 && e.Seqno != 1 {
				t.Errorf("the first edge's seqno is %d: a request's events are numbered from 1, so the counter can see a loss before the first", e.Seqno)
			}
			if i > 0 && (e.Seqno != prev.Seqno+1 || e.At < prev.At) {
				t.Errorf("edge %d: seqno %d at %v after seqno %d at %v", i, e.Seqno, e.At, prev.Seqno, prev.At)
			}
			prev = e
		}
	})
	if h.Capacity == 0 {
		return
	}
	t.Run("a full Edges drops rather than blocks, and a drop shows as a seqno gap", func(t *testing.T) {
		s := h.Open(t)
		defer s.Close()
		levels := func(n int, from meter.Level) []meter.Level {
			var out []meter.Level
			for range n {
				from = map[meter.Level]meter.Level{meter.Open: meter.Closed, meter.Closed: meter.Open}[from]
				out = append(out, from)
			}
			return out
		}
		driven := make(chan struct{})
		more := levels(h.Capacity+2, s.Initial())
		go func() { defer close(driven); h.Drive(t, s, more...) }()
		select {
		case <-driven:
		case <-time.After(Wait):
			t.Fatalf("driving %d edges into a Source holding %d blocked", len(more), h.Capacity)
		}
		var last meter.Edge
		for range h.Capacity {
			if e, ok := Receive(t, s); ok {
				last = e
			}
		}
		h.Drive(t, s, levels(1, more[len(more)-1])...)
		e, ok := Receive(t, s)
		if !ok {
			t.Fatal("Edges closed")
		}
		if e.Seqno <= last.Seqno+1 {
			t.Errorf("after %d dropped edges, seqno %d follows %d: no gap to count as lost", len(more)-h.Capacity, e.Seqno, last.Seqno)
		}
	})
}
