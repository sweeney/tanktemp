package meter

import "testing"

// Review on #9. An opening is held like a closing: one that recloses within
// Hold is a spike ("short"), and the litre in progress carries on. Without
// this, a 1 ms open spike while the pointer rests on the magnet ended the
// litre and its reclosing counted a second one, with every counter at 0.
func TestOpenSpikeDuringAPulseIsShort(t *testing.T) {
	k, _ := New(cfg, Open)
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	k.Tick(1100 * ms) // litre 1
	evs := k.Observe(Edge{At: 5000 * ms, Level: Open, Seqno: 2})
	evs = append(evs, k.Observe(Edge{At: 5001 * ms, Level: Closed, Seqno: 3})...)
	evs = append(evs, k.Tick(6000*ms)...)
	s := k.Snapshot()
	if len(evs) != 0 || s.Count != 1 || s.Short != 1 || s.Glitches != 0 || s.LastClosed != nil || s.Level != Closed {
		t.Fatalf("after the spike: events %s, snapshot %+v; want nothing, count 1, short 1", kinds(evs), s)
	}
	// The real opening ends litre 1, measured from its real closing.
	evs = k.Observe(Edge{At: 8000 * ms, Level: Open, Seqno: 4})
	evs = append(evs, k.Tick(8100*ms)...)
	if s := k.Snapshot(); kinds(evs) != "open" || evs[0].At != 8000*ms || *s.LastClosed != 7000*ms {
		t.Errorf("the real opening: events %s, snapshot %+v; want open at 8s, closed 7s", kinds(evs), s)
	}
}

// The opening that ends a litre is reported once it has held for Hold,
// stamped at the opening edge itself.
func TestOpeningIsHeld(t *testing.T) {
	k, _ := New(cfg, Open)
	evs := append(k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1}), k.Observe(Edge{At: 1400 * ms, Level: Open, Seqno: 2})...)
	if kinds(evs) != "pulse" {
		t.Fatalf("events = %s, want only the pulse while the opening is held", kinds(evs))
	}
	if evs := k.Tick(1419 * ms); len(evs) != 0 {
		t.Errorf("reported before its hold: %s", kinds(evs))
	}
	evs = k.Tick(1420 * ms)
	if kinds(evs) != "open" || evs[0].At != 1400*ms || *evs[0].Snap.LastClosed != 400*ms || evs[0].Snap.Level != Open {
		t.Errorf("events = %s %+v", kinds(evs), evs)
	}
}

// A gap while a counted litre is still closed: its end is unknown, so no
// open is reported and no closed time is measured across the gap.
func TestGapDuringACountedLitre(t *testing.T) {
	k, _ := New(cfg, Open)
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	k.Tick(1020 * ms)
	evs := k.Observe(Edge{At: 5000 * ms, Level: Closed, Seqno: 4})
	evs = append(evs, k.Observe(Edge{At: 5400 * ms, Level: Open, Seqno: 5})...)
	evs = append(evs, k.Tick(6000*ms)...)
	if s := k.Snapshot(); len(evs) != 0 || s.LastClosed != nil || s.Count != 1 || s.Lost != 2 {
		t.Errorf("events %s, snapshot %+v; want no open, last closed nil", kinds(evs), s)
	}
}

// The kernel numbers a request's events from 1, so a first edge numbered 3
// means two were dropped before it.
func TestLossBeforeTheFirstEdge(t *testing.T) {
	k, _ := New(cfg, Open)
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 3})
	if s := k.Snapshot(); s.Lost != 2 {
		t.Errorf("lost = %d, want 2", s.Lost)
	}
}

// min_interval is judged from closing to closing, not to when the hold
// confirmed it: here 590 ms, a glitch, though confirmed 1 s after.
func TestMinIntervalIsJudgedAtTheClosing(t *testing.T) {
	k, _ := feed(t, cfg, Open, at(1000*ms, Closed), at(1400*ms, Open), at(1590*ms, Closed), at(2000*ms, Open))
	k.Tick(3000 * ms)
	if s := k.Snapshot(); s.Count != 1 || s.Glitches != 1 {
		t.Errorf("snapshot = %+v, want count 1, glitches 1", s)
	}
}

// Any edge confirms a held closing, a same-level one included (WIRE:
// "at every edge"), not only a tick.
func TestASameLevelEdgeConfirmsAHeldClosing(t *testing.T) {
	k, _ := New(cfg, Open)
	k.Observe(Edge{At: 1000 * ms, Level: Closed, Seqno: 1})
	evs := k.Observe(Edge{At: 1030 * ms, Level: Closed, Seqno: 2})
	if kinds(evs) != "pulse" || evs[0].At != 1000*ms {
		t.Errorf("events = %s, want the pulse at 1s", kinds(evs))
	}
}
