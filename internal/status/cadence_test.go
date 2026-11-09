package status

import (
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/cadence"
)

// Issue #1: the tracker carries the cadence's state for /api/status, from
// the Options before the first sweep and from RecordCadence after.
func TestSnapshotCarriesTheCadence(t *testing.T) {
	initial := cadence.Status{Enabled: true, Mode: cadence.Slow, Interval: interval}
	tr := New(Options{Start: start, Interval: interval, Cadence: initial})
	snap := tr.Snapshot()
	if snap.Cadence != initial || !snap.NextDue.IsZero() {
		t.Errorf("before any sweep: cadence %+v, next due %v; want %+v and unknown", snap.Cadence, snap.NextDue, initial)
	}

	trig := &cadence.Trigger{At: start, DeviceID: "cyl", Position: "bottom", ROM: "28-000000000001", CPerMin: -3.2}
	tr.RecordCadence(cadence.Status{Enabled: true, Mode: cadence.Hold, Interval: 10 * time.Second, LastTrigger: trig, FastEntries: 1})
	due := start.Add(10 * time.Second)
	tr.SetNextDue(due)
	snap = tr.Snapshot()
	if snap.Cadence.Mode != cadence.Hold || snap.Cadence.Interval != 10*time.Second || snap.Cadence.FastEntries != 1 ||
		snap.Cadence.LastTrigger == nil || *snap.Cadence.LastTrigger != *trig || !snap.NextDue.Equal(due) {
		t.Errorf("after recording: %+v, next due %v", snap.Cadence, snap.NextDue)
	}

	// The snapshot is a copy: changing it, or the trigger it was recorded
	// from, changes nothing in the tracker.
	snap.Cadence.LastTrigger.CPerMin = 99
	trig.CPerMin = 98
	if got := tr.Snapshot().Cadence.LastTrigger.CPerMin; got != -3.2 {
		t.Errorf("the tracker's trigger was changed through a copy: %v", got)
	}
}

// Staleness stays in slow intervals (WIRE.md, Cadence): a fast cadence only
// makes sweeps fresher, so it must never make health stricter.
func TestFastCadenceDoesNotChangeStaleness(t *testing.T) {
	tr := newTracker(true)
	tr.RecordSweep(okSweep(start))
	tr.RecordCadence(cadence.Status{Enabled: true, Mode: cadence.Hold, Interval: 10 * time.Second})
	// 80 s is 8 fast intervals but under 3 slow ones (90 s): not stale.
	if h := tr.Snapshot().Health(start.Add(80 * time.Second)); h.Status != StatusOK {
		t.Errorf("80 s after a sweep, fast cadence: %s %v, want ok", h.Status, h.Detail)
	}
}

// A tracker made without Options.Cadence still reports a real cadence:
// slow, at the slow interval, never an empty mode.
func TestNewDefaultsTheCadenceToSlow(t *testing.T) {
	got := New(Options{Start: start, Interval: interval}).Snapshot().Cadence
	if got.Mode != cadence.Slow || got.Interval != interval || got.Enabled {
		t.Errorf("default cadence = %+v, want slow at %v, not adaptive", got, interval)
	}
}
