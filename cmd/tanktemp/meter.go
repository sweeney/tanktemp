package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/gpioedge"
	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/payload"
	"github.com/sweeney/tanktemp/internal/web"
)

// meterQueue bounds the meter messages waiting for MQTT: about 8 minutes
// of litres at the meter's maximum flow (two messages a litre).
const meterQueue = 1024

// meterDeps is what the meter takes from run.
type meterDeps struct {
	Pub    publisher
	Prefix string
	Wall   func() time.Time     // published times
	Mono   func() time.Duration // the edges' clock (CLOCK_MONOTONIC), to turn their times into wall times
	Synced func() bool
	Ticks  <-chan time.Duration // "now" on Mono, every 50 ms; nil: none (edges and sweeps still confirm)
	Logger *log.Logger
	// OnStop is told if the line stops (the meter is then not counting):
	// run makes health degraded with it. nil: just logged.
	OnStop func(error)
}

// meterRun is the water meter inside tanktemp (docs/WIRE.md, Meter): its
// line's Loop, and its messages on {prefix}/meter.
//
// Every message comes from the loop's goroutine, in the counter's order: a
// litre, the opening that ends it, and a sweep's state (asked for with
// Loop.Sweep, so it can't be overtaken by a litre counted meanwhile). They
// queue for one publisher goroutine, so a slow broker never stalls the
// counting, which numbers each as it publishes it: seq has no gaps. The
// queue is bounded: full, its oldest litre message goes (counted, for
// /healthz); a sweep message replaces any sweep message still waiting. No
// litre is lost with a message: each carries the cumulative count, so a
// consumer sees missed litre messages as a jump in count.
type meterRun struct {
	cfg   config.Meter
	d     meterDeps
	since time.Time
	loop  *meter.Loop

	sweepAt time.Time // the sweep being asked for: written before, read on, the loop's goroutine
	seq     uint64    // the publisher's alone

	mu    sync.Mutex
	queue []payload.MeterInput
	busy  bool // a message is being published
	drops int
	wake  chan struct{}

	// The loop and the publisher stop in that order, and only once each:
	// at SHUTDOWN finish stops the loop (no more litres), flushes, then
	// stops the publisher; otherwise Run does when its ctx ends. Run
	// returns only once both have, so no meter Publish can follow Run, or
	// follow finish (and so SHUTDOWN): mqttpub's Close chooses a clean
	// disconnect or the last will from the last Publish on any goroutine.
	stopLoop, stopPub context.CancelFunc
	loopCtx, pubCtx   context.Context
	loopDone, pubDone chan struct{}
}

// newMeter is the meter on src, counting from now.
func newMeter(cfg config.Meter, src meter.Source, d meterDeps) (*meterRun, error) {
	k, err := meter.New(meter.Config{Hold: cfg.Hold, MinInterval: cfg.MinInterval}, src.Initial())
	if err != nil {
		return nil, err
	}
	m := &meterRun{cfg: cfg, d: d, since: d.Wall(), wake: make(chan struct{}, 1),
		loopDone: make(chan struct{}), pubDone: make(chan struct{})}
	m.loopCtx, m.stopLoop = context.WithCancel(context.Background())
	m.pubCtx, m.stopPub = context.WithCancel(context.Background())
	m.loop = meter.NewLoop(k, src, d.Ticks, m.emit)
	return m, nil
}

// Run counts and publishes until ctx ends or finish stops it, and returns
// once its publisher has stopped. A line that stops is logged and reported
// (OnStop); what's queued is still published until then.
func (m *meterRun) Run(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			m.stopLoop()
			m.stopPub()
		case <-m.pubDone:
		}
	}()
	go func() { defer close(m.pubDone); m.publisher() }()
	err := m.loop.Run(m.loopCtx)
	close(m.loopDone)
	if err != nil {
		m.d.Logger.Printf("meter %s: %v; not counting until tanktemp restarts", m.cfg.DeviceID, err)
		if m.d.OnStop != nil {
			m.d.OnStop(err)
		}
	}
	<-m.pubDone
}

// finish is the meter's part of SHUTDOWN: a last sweep-type message as of
// now (at, on the wall clock), then the loop stopped, so no litre can
// follow; then everything queued published, waiting at most timeout; then
// the publisher stopped. Once it returns, no meter message can be
// published, so SHUTDOWN is the last word on the status topic's side too.
func (m *meterRun) finish(at time.Time, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	m.sweep(ctx, at)
	m.stopLoop()
	select {
	case <-m.loopDone:
	case <-ctx.Done():
	}
	for {
		m.mu.Lock()
		idle := len(m.queue) == 0 && !m.busy
		m.mu.Unlock()
		if idle {
			break
		}
		select {
		case <-ctx.Done():
			m.d.Logger.Printf("meter %s: %d messages not published by shutdown", m.cfg.DeviceID, m.queued())
		case <-time.After(5 * time.Millisecond):
			continue
		}
		break
	}
	m.stopPub()
	<-m.pubDone // a Publish in flight ends within mqttpub's own timeout
}

// emit is the Loop's: a counted litre, the opening that ends one, or a
// sweep's state. Each message's times are converted with one reading of
// the clocks, so a pulse's timestamp and last_pulse_at agree exactly.
func (m *meterRun) emit(e meter.Event) {
	off := m.offset()
	switch e.Kind {
	case meter.Pulse:
		m.publishAt(payload.TriggerPulse, off.Add(e.At), e.Snap, off)
	case meter.Opened:
		m.publishAt(payload.TriggerOpen, off.Add(e.At), e.Snap, off)
	case meter.Swept:
		m.publishAt(payload.TriggerSweep, m.sweepAt, e.Snap, off)
	}
}

// sweep publishes the meter as of a sweep's start, stamped like its states.
func (m *meterRun) sweep(ctx context.Context, at time.Time) {
	m.sweepAt = at
	m.loop.Sweep(ctx, m.d.Mono())
}

// clockOffset is wall time at mono 0, read once: wall now − mono now.
type clockOffset time.Time

// Add is the wall time of mono.
func (o clockOffset) Add(mono time.Duration) time.Time { return time.Time(o).Add(mono) }

func (m *meterRun) offset() clockOffset {
	return clockOffset(m.d.Wall().Add(-m.d.Mono()))
}

// wallOf turns a time on the edges' clock into wall time.
func (m *meterRun) wallOf(mono time.Duration) time.Time { return m.offset().Add(mono) }

// publish queues a message (numbered when it's published).
func (m *meterRun) publish(tr payload.Trigger, at time.Time, s meter.Snapshot) {
	m.publishAt(tr, at, s, m.offset())
}

func (m *meterRun) publishAt(tr payload.Trigger, at time.Time, s meter.Snapshot, off clockOffset) {
	in := payload.MeterInput{
		DeviceID: m.cfg.DeviceID, Trigger: tr, At: at, ClockSynced: m.d.Synced(),
		LitresPerPulse: m.cfg.LitresPerPulse, Level: s.Level, CountingSince: m.since, Count: s.Count,
		LastInterval: s.LastInterval, LastClosed: s.LastClosed,
		Noise: s.Noise, Short: s.Short, Glitches: s.Glitches, Lost: s.Lost,
	}
	if s.LastPulseAt != nil {
		w := off.Add(*s.LastPulseAt)
		in.LastPulseAt = &w
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if tr == payload.TriggerSweep {
		for i, old := range m.queue {
			if old.Trigger == payload.TriggerSweep {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
	}
	if len(m.queue) == meterQueue {
		m.queue = m.queue[1:]
		m.drops++
	}
	m.queue = append(m.queue, in)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// publisher sends the queue, in order, until stopPub; it checks before
// taking each message, so it stops between messages, not after the queue.
func (m *meterRun) publisher() {
	for {
		if m.pubCtx.Err() != nil {
			return
		}
		m.mu.Lock()
		if len(m.queue) == 0 {
			m.mu.Unlock()
			select {
			case <-m.pubCtx.Done():
				return
			case <-m.wake:
			}
			continue
		}
		in := m.queue[0]
		m.queue = m.queue[1:]
		m.busy = true
		m.mu.Unlock()
		m.seq++
		in.Seq = m.seq
		msg := mqttpub.Message{Topic: payload.MeterTopic(m.d.Prefix), Payload: string(payload.Meter(in)), Retained: true}
		if err := m.d.Pub.Publish(msg); err != nil {
			m.d.Logger.Printf("meter %s: publish: %v", m.cfg.DeviceID, err)
		}
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}
}

func (m *meterRun) queued() int  { m.mu.Lock(); defer m.mu.Unlock(); return len(m.queue) }
func (m *meterRun) dropped() int { m.mu.Lock(); defer m.mu.Unlock(); return m.drops }

// meterStatus is the meter as /healthz and /api/status report it: made
// before the HTTP server starts, so it reads "starting" until the line is
// requested, then counting, failed or stopped.
type meterStatus struct {
	cfg config.Meter

	mu     sync.Mutex
	state  string
	reason string
	mr     *meterRun
}

// newMeterStatus is nil with no meter (report then says []).
func newMeterStatus(cfgs []config.Meter) *meterStatus {
	if len(cfgs) == 0 {
		return nil
	}
	return &meterStatus{cfg: cfgs[0], state: web.MeterStarting}
}

func (v *meterStatus) counting(mr *meterRun) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.state, v.mr = web.MeterCounting, mr
}

func (v *meterStatus) fail(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.state, v.reason = web.MeterFailed, err.Error()
}

func (v *meterStatus) stop(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.state, v.reason = web.MeterStopped, err.Error()
}

// report is web.Server.Meters.
func (v *meterStatus) report() []web.MeterStatus {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	state, reason, mr := v.state, v.reason, v.mr
	v.mu.Unlock()
	out := web.MeterStatus{DeviceID: v.cfg.DeviceID, Line: fmt.Sprintf("%s:%d", v.cfg.Chip, *v.cfg.GPIO), State: state}
	if reason != "" {
		out.Reason = &reason
	}
	if mr == nil {
		return []web.MeterStatus{out}
	}
	out.PublishDropped, out.Queued = mr.dropped(), mr.queued()
	if state != web.MeterCounting {
		return []web.MeterStatus{out}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, ok := mr.loop.Snapshot(ctx, mr.d.Mono())
	if !ok {
		return []web.MeterStatus{out} // stopping: OnStop is about to say why
	}
	i64 := func(n uint64) *int64 { v := int64(n); return &v }
	level, since := s.Level.String(), mr.since.UTC().Format(time.RFC3339)
	out.Level, out.Count, out.CountingSince = &level, i64(s.Count), &since
	out.Noise, out.Short, out.Glitches, out.Lost = i64(s.Noise), i64(s.Short), i64(s.Glitches), i64(s.Lost)
	if s.LastPulseAt != nil {
		p := mr.wallOf(*s.LastPulseAt).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		out.LastPulseAt = &p
	}
	return []web.MeterStatus{out}
}

// lineSettings is a meter's line as gpioedge requests it: every pin and
// electrical fact from the config.
func lineSettings(m config.Meter) gpioedge.Settings {
	return gpioedge.Settings{
		Chip: m.Chip, Offset: *m.GPIO, Debounce: m.Debounce,
		Bias:       map[string]gpioedge.Bias{config.BiasPullUp: gpioedge.PullUp, config.BiasPullDown: gpioedge.PullDown, config.BiasNone: gpioedge.None}[m.Bias],
		ClosedHigh: m.ClosedLevel == config.ClosedHigh,
	}
}
