package cadence

import "time"

// NextDue is rule 4, the scheduling arithmetic, kept here so it's pure and
// tested on its own; cmd/tanktemp's scheduler owns the timer.
//
// anchor is when the previous tick was due, the moment the sweep that just
// finished was meant to start. The next tick is due one interval after it,
// as a time.Ticker would be. Anchoring on the tick, not on when the sweep
// actually began, means scheduling latency can't accumulate.
//
// If that moment has already passed by now (the sweep overran), the ticks
// it missed are skipped, not queued: due is the first anchor + k×interval
// after now, and skipped is how many were passed over (k − 1). A tick
// exactly at now counts as missed, as one lifecycle would have seen arrive
// while the sweep was still running. That's what lifecycle did with a
// ticker before issue #1, and what it still does with any tick that
// arrives during a sweep.
//
// It uses only Add and Sub. Times from time.Now carry a monotonic clock
// reading, and Add keeps it, so a wall-clock step (NTP at boot, on a Pi
// with no real-time clock) can't move the schedule: the scheduler's times
// must come straight from time.Now, never through UTC, Round or Truncate,
// which strip it. The skipped count is computed, not looped, so even a
// large gap costs nothing.
//
// interval must be positive; config guarantees it. A non-positive one gives
// "now", so the next sweep follows at once rather than never.
func NextDue(anchor time.Time, interval time.Duration, now time.Time) (due time.Time, skipped int) {
	if interval <= 0 {
		return now, 0
	}
	due = anchor.Add(interval)
	if due.After(now) {
		return due, 0
	}
	n := now.Sub(due)/interval + 1 // ticks at or before now, after anchor
	return due.Add(n * interval), int(n)
}
