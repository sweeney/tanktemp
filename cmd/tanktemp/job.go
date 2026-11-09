package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/tanktemp/internal/cadence"
	"github.com/sweeney/tanktemp/internal/payload"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// sweepJob is lifecycle's unit of work: one sweep, then everything that
// follows from it. It was run's Work closure until it grew enough to need
// its own tests (review on #4). Its dependencies are small interfaces
// declared here, where they're used (CLAUDE.md's Publisher pattern), so
// job_test.go drives every path with hand-written fakes and no run.
type sweepJob struct {
	sweeper interface {
		Sweep(context.Context) (sweep.Sweep, error)
	}
	cadence interface {
		Observe(sweep.Sweep, time.Time) cadence.Decision
		Status() cadence.Status
	}
	tracker interface {
		RecordSweep(sweep.Sweep)
		RecordCadence(cadence.Status)
	}
	pub     publisher
	prefix  string
	logger  *log.Logger // always-on lines: the fault line
	verbose *log.Logger // bench-only lines (log.verbose); never nil

	// interval is what the last completed sweep's cadence decided: the gap
	// to the next sweep. Work writes it on lifecycle's work goroutine;
	// lifecycle's Next reads it (Interval) on its own goroutine, but only
	// after it has received that Work's result, which orders the two.
	interval time.Duration
}

// Work is one sweep (lifecycle.Config.Work). started is lifecycle's start
// time for this unit, from deps.Now (time.Now in production, so monotonic):
// the cadence times rates and its curve with it, so a wall-clock step can't
// move them, while the sweep's own timestamp (sw.At) is what's published.
//
// In order: sweep; observe (the cadence decides the next interval); record
// both for /healthz and /api/status; verbose sweep line; publish each
// tank's state; the fault line, if any; the verbose cadence line, if any
// (it's what this sweep decided about the next one, so it goes last).
//
// A failed sweep (only ErrCancelled, at shutdown) observes, records and
// publishes nothing, and leaves Interval as the last sweep decided. A
// publish error doesn't undo the rest: the sweep happened, and the cadence
// moved on with it.
func (j *sweepJob) Work(ctx context.Context, started time.Time) error {
	sw, err := j.sweeper.Sweep(ctx)
	if err != nil {
		return err
	}
	decision := j.cadence.Observe(sw, started)
	j.interval = decision.Interval
	j.tracker.RecordSweep(sw)
	j.tracker.RecordCadence(j.cadence.Status())

	// Verbose: every sweep, one line, from the sweep already in hand. It
	// goes before the fault line, so with verbose on a faulty sweep reads
	// as its values, then what was wrong with them.
	j.verbose.Print(sw.Line())
	var errs []error
	for _, t := range sw.Tanks {
		if err := j.pub.Publish(mqttpub.Message{
			Topic: payload.StateTopic(j.prefix, t.DeviceID), Payload: string(payload.State(sw, t)), Retained: true,
		}); err != nil {
			errs = append(errs, fmt.Errorf("publish %s: %w", t.DeviceID, err))
		}
	}
	if faults := sw.Faults(); faults != "" {
		ok, failed := sw.Counts()
		j.logger.Printf("sweep: %d ok, %d failed, %s: %s", ok, failed, sw.Took(), faults)
	}
	if line := decision.Line(); line != "" {
		j.verbose.Print(line)
	}
	return errors.Join(errs...)
}

// Interval is the gap the last completed sweep decided (see the field).
func (j *sweepJob) Interval() time.Duration { return j.interval }

// nextSweep is lifecycle's Config.Next for the adaptive cadence (rule 4).
// lifecycle calls it on its own goroutine once it has seen a sweep finish,
// so it can't race the sweep, and a sweep can never be due while the one
// before is finishing (common lifecycle's design, decision 6). It asks the
// Schedule for the next due time from lifecycle's start for the sweep just
// done and the interval that sweep decided, counts any due times an overrun
// passed (onSkip: skipped_ticks), and tells the tracker when the next is
// due (onDue: /api/status's next_due).
//
// now is lifecycle's clock, deps.Now: in production time.Now, so started,
// now and every due time carry the monotonic reading, and a wall-clock step
// can't move the schedule.
func nextSweep(s *cadence.Schedule, now func() time.Time, interval func() time.Duration,
	onDue func(time.Time), onSkip func()) func(time.Time, error) time.Time {
	return func(started time.Time, _ error) time.Time {
		due, skipped := s.Next(started, now(), interval())
		for range skipped {
			onSkip()
		}
		onDue(due)
		return due
	}
}
