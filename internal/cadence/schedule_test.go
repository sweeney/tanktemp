package cadence

import (
	"testing"
	"time"
)

// Rule 4: the next sweep is due one interval after the previous tick, and
// a sweep that overruns skips ticks rather than queueing them.
func TestNextDue(t *testing.T) {
	const i = 10 * time.Second
	tests := []struct {
		name    string
		now     time.Duration // since the anchor
		due     time.Duration // since the anchor
		skipped int
	}{
		{"a quick sweep", 4 * time.Second, i, 0},
		{"finished just before the tick", i - time.Millisecond, i, 0},
		// Exactly on the tick: that tick is already now, so it's missed
		// (lifecycle would have seen it arrive while the sweep ran).
		{"finished exactly on the tick", i, 2 * i, 1},
		{"overran by 2 s", 12 * time.Second, 2 * i, 1},
		{"overran three ticks", 35 * time.Second, 4 * i, 3},
	}
	for _, tt := range tests {
		due, skipped := NextDue(t0, i, t0.Add(tt.now))
		if !due.Equal(t0.Add(tt.due)) || skipped != tt.skipped {
			t.Errorf("%s: due +%v, skipped %d; want +%v, %d", tt.name, due.Sub(t0), skipped, tt.due, tt.skipped)
		}
	}
}

// A clock far past the anchor (a forward step, or a suspended machine)
// costs arithmetic, not a loop of millions of iterations.
func TestNextDueAfterALongGapIsArithmetic(t *testing.T) {
	now := t0.Add(56 * 365 * 24 * time.Hour)
	start := time.Now()
	due, skipped := NextDue(t0, 2*time.Second, now)
	if time.Since(start) > 10*time.Millisecond {
		t.Errorf("took %v", time.Since(start))
	}
	if !due.After(now) || due.Sub(now) > 2*time.Second || skipped < 800_000_000 {
		t.Errorf("due %v after now, skipped %d", due.Sub(now), skipped)
	}
}

// The scheduler's times come from time.Now and carry a monotonic reading;
// NextDue must keep it, so a wall-clock step (NTP) can't move the schedule.
func TestNextDueKeepsTheMonotonicClock(t *testing.T) {
	anchor := time.Now()
	due, _ := NextDue(anchor, time.Second, anchor)
	// String() shows "m=+…" only for a time with a monotonic reading.
	if s := due.String(); !containsMonotonic(s) {
		t.Errorf("due %s has lost the monotonic reading", s)
	}
}

func containsMonotonic(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i:i+3] == "m=+" || s[i:i+3] == "m=-" {
			return true
		}
	}
	return false
}

// A non-positive interval can't come from a valid config. It means "now",
// so a sweep follows at once, rather than a division by zero.
func TestNextDueWithNoInterval(t *testing.T) {
	now := t0.Add(time.Minute)
	if due, skipped := NextDue(t0, 0, now); !due.Equal(now) || skipped != 0 {
		t.Errorf("zero interval: %v, %d", due, skipped)
	}
}
