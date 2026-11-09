package main

import (
	"context"
	"time"

	"github.com/sweeney/tanktemp/internal/cadence"
)

// scheduler is the adaptive cadence's clock (issue #1): it feeds
// lifecycle.Config.Tick, waiting before each tick for however long the
// cadence decided after the sweep before. internal/cadence decides the
// interval and does the arithmetic (cadence.NextDue); this owns the timer,
// which is why it lives here and not in the core.
//
// # Why it waits for each sweep
//
// lifecycle starts a sweep on a tick and drops any tick that arrives while
// a sweep is still running (OnSkip). The interval to wait depends on the
// sweep that's running, so the scheduler can't arm the next tick until
// that sweep reports (Report). It then sends one tick, and waits for that
// tick's outcome. Every tick has exactly one:
//
//   - lifecycle started a sweep on it, which will Report; or
//   - lifecycle dropped it (Skipped, from lifecycle's OnSkip), because it
//     hadn't yet seen the last sweep finish. Report runs in Work's defer,
//     a moment before lifecycle learns Work has returned, so a tick due in
//     that moment is dropped although nothing is running. The scheduler
//     sends the same tick again at once: it isn't an overrun (NextDue
//     handles those), so it's neither waited on nor counted as skipped.
//     The resend can only be dropped again until lifecycle reads the
//     finished sweep, which it is already about to.
//
// Waiting for that outcome is what stops the schedule stalling or
// drifting: without Skipped, a dropped tick would leave the scheduler
// waiting for a Report that never comes, and tanktemp would never sweep
// again. In real time the window is microseconds against intervals of
// seconds; it was found in the virtual-clock tests, where time is free.
//
// # Time
//
// Each tick is due one interval after the tick before (cadence.NextDue),
// not after the sweep finished and not after it reported, so neither the
// sweep's length nor scheduling latency moves the schedule. Due times
// start from now() (time.Now in production) and only ever go through Add
// and Sub, so they keep Go's monotonic clock reading, and a wall-clock
// step (NTP syncing a Pi that has no real-time clock) can't move them.
// The cadence's own idea of time comes from the sweeps' timestamps.
type scheduler struct {
	done  <-chan struct{} // the scheduler's context: closed when it stops
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	onDue  func(time.Time) // the tracker's SetNextDue, for /api/status
	onSkip func()          // the tracker's SkippedTick, for ticks an overrun missed (NextDue)

	tick    chan time.Time     // lifecycle's Tick
	reports chan time.Duration // a sweep finished; the interval it decided
	skips   chan struct{}      // lifecycle dropped the last tick
}

// newScheduler returns a scheduler that stops when ctx is done. Run it with
// run; hand Tick to lifecycle; call Report at the end of every Work and
// Skipped from lifecycle's OnSkip.
func newScheduler(ctx context.Context, now func() time.Time, after func(time.Duration) <-chan time.Time,
	onDue func(time.Time), onSkip func()) *scheduler {
	return &scheduler{
		done: ctx.Done(), now: now, after: after, onDue: onDue, onSkip: onSkip,
		tick: make(chan time.Time),
		// Buffered so neither lifecycle's goroutine (Skipped) nor a sweep's
		// (Report) waits on the scheduler, which is only ever expecting one
		// of them at a time.
		reports: make(chan time.Duration, 1),
		skips:   make(chan struct{}, 1),
	}
}

// Tick is the channel for lifecycle.Config.Tick.
func (s *scheduler) Tick() <-chan time.Time { return s.tick }

// Report says a sweep has finished, and how long the cadence wants until the
// next one. It must be called once at the end of every Work, including one
// that failed, or the scheduler would wait for it for ever. It never blocks
// once the scheduler has stopped.
func (s *scheduler) Report(interval time.Duration) {
	select {
	case s.reports <- interval:
	case <-s.done:
	}
}

// Skipped says lifecycle dropped the last tick (lifecycle's OnSkip); the
// scheduler sends it again. It never blocks once the scheduler has stopped.
func (s *scheduler) Skipped() {
	select {
	case s.skips <- struct{}{}:
	case <-s.done:
	}
}

// run is the scheduler's loop, until its context is done. start is when the
// sweep that's already running began: lifecycle's Immediate one, which
// starts without a tick, so it's the first anchor.
func (s *scheduler) run(start time.Time) {
	anchor := start // when the tick for the sweep in progress was due
	var interval time.Duration
	// The immediate sweep is in progress; wait for its report.
	select {
	case <-s.done:
		return
	case interval = <-s.reports:
	}
	for {
		due, skipped := cadence.NextDue(anchor, interval, s.now())
		for range skipped {
			s.onSkip() // the sweep overran these ticks: skipped, not queued
		}
		s.onDue(due)
		select {
		case <-s.done:
			return
		case <-s.after(due.Sub(s.now())):
		}
		// Send the tick until lifecycle starts a sweep on it: its outcome is
		// either that sweep's report, or a drop, and then it goes again.
		for taken := false; !taken; {
			select {
			case <-s.done:
				return
			case s.tick <- due:
			}
			select {
			case <-s.done:
				return
			case interval = <-s.reports:
				taken = true
			case <-s.skips:
			}
		}
		anchor = due
	}
}
