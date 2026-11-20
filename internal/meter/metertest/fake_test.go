package metertest

import (
	"sync"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

func TestFakeMeetsContract(t *testing.T) {
	Contract(t, Harness{
		Open: func(t *testing.T) meter.Source { return NewFake(meter.Open) },
		Drive: func(t *testing.T, s meter.Source, levels ...meter.Level) {
			f := s.(*Fake)
			for i, l := range levels {
				f.Push(time.Duration(i+1)*time.Second, l)
			}
		},
	})
}

// The fake can also send any edge as given, gaps and all, for the counter's
// own tests.
func TestFakeSendsRawEdges(t *testing.T) {
	f := NewFake(meter.Closed)
	f.Send(meter.Edge{At: 5, Level: meter.Open, Seqno: 9})
	if e, _ := Receive(t, f); e.Seqno != 9 || e.At != 5 || e.Level != meter.Open {
		t.Errorf("edge = %+v", e)
	}
	f.Push(6, meter.Closed) // carries on from the last seqno sent
	if e, _ := Receive(t, f); e.Seqno != 10 {
		t.Errorf("seqno after a raw edge = %d, want 10", e.Seqno)
	}
	if f.Initial() != meter.Closed {
		t.Error("initial level")
	}
}

// Concurrent Pushes each take their own seqno (review, #3).
func TestFakeConcurrentPushesNumberUniquely(t *testing.T) {
	f := NewFake(meter.Open)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); f.Push(time.Duration(i), meter.Closed) }()
	}
	wg.Wait()
	f.Close()
	seen := map[uint64]bool{}
	for e := range f.Edges() {
		if seen[e.Seqno] {
			t.Fatalf("seqno %d twice", e.Seqno)
		}
		seen[e.Seqno] = true
	}
	if len(seen) != 50 {
		t.Errorf("%d edges, want 50", len(seen))
	}
}

// Like the real source, a full channel drops the edge rather than block:
// the reader sees a seqno gap, and Close never deadlocks (review, #3).
func TestFakeDropsWhenFull(t *testing.T) {
	f := NewFake(meter.Open)
	for i := range Capacity + 3 {
		f.Push(time.Duration(i), meter.Closed)
	}
	f.Push(time.Hour, meter.Open) // seqno Capacity+4, dropped too
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for range f.Edges() {
		n++
	}
	if n != Capacity {
		t.Errorf("%d edges delivered, want %d", n, Capacity)
	}
}
