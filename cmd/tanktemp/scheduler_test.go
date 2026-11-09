package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// virtualClock is a fake clock for the scheduler. Time moves only when
// someone waits on it: After(d) moves now on by d and fires at once, as if
// the wait had passed instantly, and records d, so a test reads the
// scheduler's waits as the gaps between sweeps. Advance moves now without
// a wait (a sweep that takes time). After a limit of waits it stops firing,
// so a run under test comes to rest instead of racing on.
type virtualClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	limit int // 0: no limit
}

func newVirtualClock(start time.Time, limit int) *virtualClock {
	return &virtualClock{now: start, limit: limit}
}

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit > 0 && len(c.waits) >= c.limit {
		return nil // never fires: the run rests here until the test stops it
	}
	if d > 0 {
		c.now = c.now.Add(d)
	}
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func (c *virtualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *virtualClock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

// schedRig is a scheduler running against a virtual clock, with what it
// tells the tracker recorded.
type schedRig struct {
	clock  *virtualClock
	s      *scheduler
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	dues  []time.Time
	skips int
}

func startScheduler(t *testing.T, clock *virtualClock) *schedRig {
	t.Helper()
	r := &schedRig{clock: clock, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.s = newScheduler(ctx, clock.Now, clock.After,
		func(due time.Time) { r.mu.Lock(); r.dues = append(r.dues, due); r.mu.Unlock() },
		func() { r.mu.Lock(); r.skips++; r.mu.Unlock() })
	start := clock.Now() // now, not in the goroutine: the test may Advance first
	go func() { r.s.run(start); close(r.done) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func (r *schedRig) nextTick(t *testing.T) time.Time {
	t.Helper()
	select {
	case at := <-r.s.Tick():
		return at
	case <-time.After(2 * time.Second):
		t.Fatal("no tick")
	}
	return time.Time{}
}

func (r *schedRig) noTick(t *testing.T) {
	t.Helper()
	select {
	case at := <-r.s.Tick():
		t.Fatalf("unexpected tick at %v", at)
	case <-time.After(20 * time.Millisecond):
	}
}

func (r *schedRig) recorded() ([]time.Time, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.dues...), r.skips
}

// Nothing ticks until the sweep that's running (lifecycle's immediate one)
// reports; then each tick is one reported interval after the one before.
func TestSchedulerTicksOneIntervalAfterTheLast(t *testing.T) {
	clock := newVirtualClock(t0, 0)
	r := startScheduler(t, clock)
	r.noTick(t)

	r.s.Report(30 * time.Second) // the immediate sweep, started at t0
	if at := r.nextTick(t); !at.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("first tick at +%v, want +30s", at.Sub(t0))
	}
	r.noTick(t) // and no more until that sweep reports

	clock.Advance(4 * time.Second) // the sweep takes 4 s; the gap still runs from its tick
	r.s.Report(10 * time.Second)   // it saw a draw: fast
	if at := r.nextTick(t); !at.Equal(t0.Add(40 * time.Second)) {
		t.Fatalf("second tick at +%v, want +40s (10 s after the tick at +30s)", at.Sub(t0))
	}
	dues, skips := r.recorded()
	if len(dues) != 2 || !dues[0].Equal(t0.Add(30*time.Second)) || !dues[1].Equal(t0.Add(40*time.Second)) || skips != 0 {
		t.Errorf("told the tracker due %v, %d skips", dues, skips)
	}
}

// Rule 4: a sweep that overruns its interval skips the ticks it missed,
// counting them, rather than starting the next sweep at once to catch up.
func TestSchedulerSkipsTicksAnOverrunMissed(t *testing.T) {
	clock := newVirtualClock(t0, 0)
	r := startScheduler(t, clock)
	clock.Advance(25 * time.Second) // the immediate sweep takes 25 s...
	r.s.Report(10 * time.Second)    // ...against a 10 s interval: +10 and +20 have passed
	if at := r.nextTick(t); !at.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("tick at +%v, want +30s", at.Sub(t0))
	}
	if _, skips := r.recorded(); skips != 2 {
		t.Errorf("%d skipped ticks counted, want 2", skips)
	}
}

// A tick lifecycle dropped (it arrived before lifecycle had seen the last
// sweep finish) has no sweep to report on it. Skipped says so, and the
// next tick follows one interval later, so the schedule never stalls.
func TestSchedulerCarriesOnAfterADroppedTick(t *testing.T) {
	clock := newVirtualClock(t0, 0)
	r := startScheduler(t, clock)
	r.s.Report(10 * time.Second)
	r.nextTick(t) // +10s: dropped by lifecycle
	r.s.Skipped()
	if at := r.nextTick(t); !at.Equal(t0.Add(20 * time.Second)) {
		t.Fatalf("after a dropped tick, the next is at +%v, want +20s", at.Sub(t0))
	}
}

// Report and Skipped never block once the scheduler has stopped, so a
// sweep finishing during shutdown can't hang lifecycle.
func TestSchedulerStopsCleanly(t *testing.T) {
	r := startScheduler(t, newVirtualClock(t0, 0))
	r.cancel()
	<-r.done
	finished := make(chan struct{})
	go func() {
		r.s.Report(time.Second)
		r.s.Report(time.Second)
		r.s.Skipped()
		r.s.Skipped()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Report or Skipped blocked after the scheduler stopped")
	}
}

// With the real clock: time.After, monotonic times, a real wait.
func TestSchedulerOnTheRealClock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newScheduler(ctx, time.Now, time.After, func(time.Time) {}, func() {})
	begin := time.Now()
	go s.run(begin)
	s.Report(30 * time.Millisecond)
	select {
	case <-s.Tick():
		if took := time.Since(begin); took < 30*time.Millisecond || took > time.Second {
			t.Errorf("ticked after %v, want about 30ms", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tick on the real clock")
	}
}
