package cadence

import "time"

// NextDue is rule 4, the scheduling arithmetic, kept here so it's pure and
// tested on its own; Schedule uses it, and lifecycle owns the timer.
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
// with no real-time clock) can't move the schedule: its times must come
// straight from time.Now (lifecycle's started and Now), never through UTC,
// Round or Truncate, which strip it. The skipped count is computed, not looped, so even a
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

// Schedule is lifecycle's Config.Next for the adaptive cadence: after each
// sweep, lifecycle (on its own goroutine, once it has seen the sweep
// finish) asks when the next one is due, and tanktemp asks this.
//
// The first sweep's start anchors a grid; after that each due time is one
// interval after the previous due time (NextDue), not after the sweep
// actually started, so lifecycle's few ms of latency per sweep can't
// accumulate. The zero value is ready to use. It isn't safe for concurrent
// use; lifecycle never calls Next concurrently.
type Schedule struct {
	anchor time.Time // the last due time; zero before the first sweep
}

// Next is the due time of the next sweep, given when the last one started
// (lifecycle's started), now, and the interval the cadence decided after
// it. skipped counts the due times an overrun passed (NextDue). A zero
// started (lifecycle asking for the first sweep without Immediate, which
// tanktemp never does) gives now.
func (s *Schedule) Next(started, now time.Time, interval time.Duration) (due time.Time, skipped int) {
	if started.IsZero() {
		return now, 0
	}
	if s.anchor.IsZero() {
		s.anchor = started
	}
	due, skipped = NextDue(s.anchor, interval, now)
	s.anchor = due
	return due, skipped
}
