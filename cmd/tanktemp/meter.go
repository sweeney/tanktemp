package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/payload"
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
}

// meterRun is the water meter inside tanktemp (docs/WIRE.md, Meter): its
// line's Loop, and its messages on {prefix}/meter.
//
// Messages are built on the goroutine that has them (the loop's, for a
// litre and its opening; the sweep's, for a sweep) and numbered there from
// one series, then handed to a queue that one goroutine publishes from, so
// a slow broker never stalls the counting. The queue is bounded: when it is
// full the oldest litre message goes (and is counted, for /healthz); a
// sweep message replaces any sweep message still waiting. No litre is lost
// with a message: each carries the cumulative count, and consumers see the
// seq gap.
type meterRun struct {
	cfg   config.Meter
	d     meterDeps
	since time.Time
	loop  *meter.Loop

	mu    sync.Mutex
	seq   uint64
	queue []queued
	drops int
	wake  chan struct{}
}

type queued struct {
	msg   mqttpub.Message
	sweep bool
}

// newMeter is the meter on src, counting from now.
func newMeter(cfg config.Meter, src meter.Source, d meterDeps) (*meterRun, error) {
	k, err := meter.New(meter.Config{Hold: cfg.Hold, MinInterval: cfg.MinInterval}, src.Initial())
	if err != nil {
		return nil, err
	}
	m := &meterRun{cfg: cfg, d: d, since: d.Wall(), wake: make(chan struct{}, 1)}
	m.loop = meter.NewLoop(k, src, d.Ticks, m.emit)
	return m, nil
}

// Run counts and publishes until ctx ends or the line stops (logged).
func (m *meterRun) Run(ctx context.Context) {
	go m.publisher(ctx)
	if err := m.loop.Run(ctx); err != nil {
		m.d.Logger.Printf("meter %s: %v; not counting until tanktemp restarts", m.cfg.DeviceID, err)
	}
}

// emit is the Loop's: a counted litre or the opening that ends one.
func (m *meterRun) emit(e meter.Event) {
	tr := payload.TriggerPulse
	if e.Kind == meter.Opened {
		tr = payload.TriggerOpen
	}
	m.publish(tr, m.wallOf(e.At), e.Snap)
}

// sweep publishes the meter as of a sweep's start, stamped like its states.
func (m *meterRun) sweep(ctx context.Context, at time.Time) {
	if s, ok := m.loop.Snapshot(ctx, m.d.Mono()); ok {
		m.publish(payload.TriggerSweep, at, s)
	}
}

// wallOf turns a time on the edges' clock into wall time.
func (m *meterRun) wallOf(mono time.Duration) time.Time {
	return m.d.Wall().Add(mono - m.d.Mono())
}

// publish numbers a message and queues it.
func (m *meterRun) publish(tr payload.Trigger, at time.Time, s meter.Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	in := payload.MeterInput{
		DeviceID: m.cfg.DeviceID, Trigger: tr, At: at, ClockSynced: m.d.Synced(), Seq: m.seq,
		LitresPerPulse: m.cfg.LitresPerPulse, Level: s.Level, CountingSince: m.since, Count: s.Count,
		LastInterval: s.LastInterval, LastClosed: s.LastClosed,
		Noise: s.Noise, Short: s.Short, Glitches: s.Glitches, Lost: s.Lost,
	}
	if s.LastPulseAt != nil {
		w := m.wallOf(*s.LastPulseAt)
		in.LastPulseAt = &w
	}
	q := queued{msg: mqttpub.Message{Topic: payload.MeterTopic(m.d.Prefix), Payload: string(payload.Meter(in)), Retained: true}, sweep: tr == payload.TriggerSweep}
	if q.sweep {
		for i, old := range m.queue {
			if old.sweep {
				m.queue = append(m.queue[:i], m.queue[i+1:]...)
				break
			}
		}
	}
	if len(m.queue) == meterQueue {
		m.queue = m.queue[1:]
		m.drops++
	}
	m.queue = append(m.queue, q)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// publisher sends the queue, in order, until ctx ends.
func (m *meterRun) publisher(ctx context.Context) {
	for {
		m.mu.Lock()
		if len(m.queue) == 0 {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			}
			continue
		}
		q := m.queue[0]
		m.queue = m.queue[1:]
		m.mu.Unlock()
		if err := m.d.Pub.Publish(q.msg); err != nil {
			m.d.Logger.Printf("meter %s: publish: %v", m.cfg.DeviceID, err)
		}
	}
}

func (m *meterRun) queued() int  { m.mu.Lock(); defer m.mu.Unlock(); return len(m.queue) }
func (m *meterRun) dropped() int { m.mu.Lock(); defer m.mu.Unlock(); return m.drops }
