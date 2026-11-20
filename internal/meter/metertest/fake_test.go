package metertest

import (
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
