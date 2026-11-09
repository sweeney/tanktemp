package cadence

import "time"

// NextDue is rule 4, the scheduling arithmetic, kept here so it's pure and
// tested on its own; Schedule uses it, and lifecycle owns the timer.
//
// anchor is when the sweep that just finished was due to start. The next
// is due one interval after it, as on a time.Ticker's grid. Anchoring on
// the due time, not on when the sweep actually began, means scheduling
// latency can't accumulate.
//
// lifecycle asks (its Config.Next) only once it has seen the sweep finish,
// so at now nothing is running. If the next due time is now or later, it
// stands. If it has already passed (the sweep overran), the due times it
// missed are skipped, not queued: due is the first anchor + k×interval at
// or after now, and skipped is how many were passed over (k − 1). A sweep
// that ends exactly on the next due time isn't an overrun: the next one
// starts at once (review on #4; with ticks it would have been missed).
//
// It uses only Add and Sub. Times from time.Now carry a monotonic clock
// reading, and Add keeps it, so a wall-clock step (NTP at boot, on a Pi
// with no real-time clock) can't move the schedule: its times must come
// straight from time.Now (lifecycle's started and Now), never through
// UTC, Round or Truncate, which strip it. The skipped count is computed,
// not looped, so even a large gap costs nothing.
//
// interval must be positive; config guarantees it. A non-positive one gives
// "now", so the next sweep follows at once rather than never.
func NextDue(anchor time.Time, interval time.Duration, now time.Time) (due time.Time, skipped int) {
	if interval <= 0 {
		return now, 0
	}
	due = anchor.Add(interval)
	if due.After(now) {
		return due, 0 // the usual case: due in the future
	}
	// due is now or past: the first due time at or after now is n
	// intervals on, n = ⌈late/interval⌉. That's the one rule for the
	// instant itself too: late = 0 gives n = 0, due stands, nothing is
	// skipped (review on #4: one guard, not two). It's the remainder test,
	// not (late+interval-1)/interval, so a late near the largest Duration
	// can't overflow.
	late := now.Sub(due)
	n := late / interval
	if late%interval != 0 {
		n++
	}
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
