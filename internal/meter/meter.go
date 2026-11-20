// Package meter counts a reed-switch water meter's pulses: one litre each
// time the reed goes from open to closed (a magnet on the meter's litre
// pointer closes it once per turn).
//
// It is core logic, as pure as sweep and cadence: no os, no clock, no GPIO,
// no MQTT. It sees time only as it's handed in, on the monotonic clock (the
// kernel's edge timestamps), so a wall-clock step can't fake or hide a
// pulse. The caller turns monotonic times into wall-clock ones to publish.
//
// It is a state machine over both edges, and never trusts an edge's label
// on its own. On the rig's first night (2026-11-19/20, no debounce) 15 of
// 277 edges were microsecond spikes, mostly as the boiler's programmer
// switched, and each reported the level the line ALREADY had ("closes" while
// closed). Counting every falling edge gave 140 litres; this counts 131. The
// rules, in the order they're checked:
//
//  1. Lost: a gap in the kernel's line seqno means it dropped edges. They
//     are counted as lost, the reported level is taken as the truth, and no
//     litre is ever counted on that resync, nor a closing still held at the
//     gap: a litre is lost honestly rather than invented. A repeated or
//     backwards seqno isn't a gap: that edge is ignored as noise, and a held
//     closing is kept.
//  2. Noise: an edge reporting the level the counter already holds changes
//     nothing.
//  3. Hold: a closing counts only once it has stayed closed for Hold, which
//     the next edge or a Tick confirms. A closing that reopens sooner was a
//     spike that flipped the reading: both edges are one "short". This is
//     belt and braces behind the kernel's debounce.
//  4. MinInterval: a held closing less than MinInterval after the last
//     COUNTED closing is a glitch, a backstop: the meter's maximum flow
//     (Q4, 83 L/min) is a pulse every 0.72 s.
//
// A counted litre is stamped at its closing edge, not when the hold
// confirmed it. The opening that ends a counted pulse reports how long the
// reed was closed, which gives the flow within a single litre (the reed is
// closed for a roughly fixed share of each turn; consumers learn the share).
package meter

import (
	"errors"
	"fmt"
	"time"
)

// Level is the reed's state as the line reports it. There is no "unknown":
// the caller reads the line's level when it requests it.
type Level int

const (
	// Open: the reed is open, the pull-up holds the line high.
	Open Level = iota + 1
	// Closed: the magnet holds the reed closed, the line is low.
	Closed
)

func (l Level) String() string {
	switch l {
	case Open:
		return "open"
	case Closed:
		return "closed"
	}
	return fmt.Sprintf("Level(%d)", int(l))
}

// Edge is one edge event: when (monotonic), the level the line was read at
// after it, and the line's event seqno.
type Edge struct {
	At    time.Duration
	Level Level
	Seqno uint64
}

// Kind is what an Event reports.
type Kind string

const (
	// Pulse is a counted litre, stamped at its closing edge.
	Pulse Kind = "pulse"
	// Opened is the opening that ends a counted pulse; Snapshot's
	// LastClosed then holds how long it was closed.
	Opened Kind = "open"
)

// Event is something a caller publishes: a counted litre, or the end of
// one, with the counter's state as of that event. (One edge can confirm a
// held litre and end it: the pulse's snapshot is still the state at the
// pulse, closed, not after the opening.)
type Event struct {
	Kind Kind
	At   time.Duration
	Snap Snapshot
}

// Config holds the user-space rules. Zero for both turns them off, leaving
// only the state machine.
type Config struct {
	// Hold is how long a closing must stay closed to count.
	Hold time.Duration
	// MinInterval is the shortest gap between counted closings.
	MinInterval time.Duration
}

// Snapshot is the counter's state, for publishing. Durations are monotonic
// times or intervals; nil means not yet known.
type Snapshot struct {
	Count uint64
	Level Level
	// LastPulseAt is the last counted closing; LastInterval the gap between
	// the last two; LastClosed how long the last counted pulse was closed.
	LastPulseAt, LastInterval, LastClosed *time.Duration
	// Rejects, by reason (see the package doc).
	Noise, Short, Glitches, Lost uint64
}

// Counter is the state machine. It is not safe for concurrent use.
type Counter struct {
	cfg     Config
	level   Level
	seq     uint64
	haveSeq bool

	pending *Edge // a closing waiting out its hold
	inPulse bool  // the current closed phase is a counted litre
	pulseAt time.Duration

	snap Snapshot
}

// New returns a counter starting at the line's level as read when it was
// requested. If the reed starts closed, that litre isn't counted.
func New(c Config, initial Level) (*Counter, error) {
	if c.Hold < 0 || c.MinInterval < 0 {
		return nil, errors.New("meter: hold and min interval can't be negative")
	}
	if c.MinInterval > 0 && c.Hold >= c.MinInterval {
		return nil, fmt.Errorf("meter: hold %v must be shorter than min interval %v", c.Hold, c.MinInterval)
	}
	if initial != Open && initial != Closed {
		return nil, fmt.Errorf("meter: initial level %v is not open or closed", initial)
	}
	k := &Counter{cfg: c, level: initial}
	k.snap.Level = initial
	return k, nil
}

// Observe takes the next edge and returns what to publish, in order.
func (k *Counter) Observe(e Edge) []Event {
	// A repeated or backwards seqno is no edge the kernel sent in order:
	// ignore it as noise, and keep any held closing.
	if k.haveSeq && e.Seqno <= k.seq {
		k.snap.Noise++
		return nil
	}
	// Then a gap: a closing still held at a gap is dropped even past its
	// hold, since the dropped edges may have reopened it.
	if k.haveSeq && e.Seqno != k.seq+1 {
		k.snap.Lost += e.Seqno - k.seq - 1
		k.seq = e.Seqno
		k.pending, k.inPulse = nil, false
		k.setLevel(e.Level)
		return nil
	}
	k.seq, k.haveSeq = e.Seqno, true

	evs := k.Tick(e.At)

	if e.Level == k.level {
		k.snap.Noise++
		return evs
	}
	if e.Level == Closed {
		k.setLevel(Closed)
		p := e
		k.pending = &p
		return append(evs, k.Tick(e.At)...) // a zero hold confirms at once
	}
	// Closed -> open.
	k.setLevel(Open)
	if k.pending != nil { // reopened within the hold
		k.pending = nil
		k.snap.Short++
		return evs
	}
	if k.inPulse {
		k.inPulse = false
		d := e.At - k.pulseAt
		k.snap.LastClosed = &d
		evs = append(evs, Event{Kind: Opened, At: e.At, Snap: k.Snapshot()})
	}
	return evs
}

// Tick confirms a held closing once now is at least Hold after it, so a
// litre counts even while the pointer rests on the magnet.
func (k *Counter) Tick(now time.Duration) []Event {
	p := k.pending
	if p == nil || now-p.At < k.cfg.Hold {
		return nil
	}
	k.pending = nil
	if last := k.snap.LastPulseAt; last != nil && k.cfg.MinInterval > 0 && p.At-*last < k.cfg.MinInterval {
		k.snap.Glitches++
		return nil
	}
	if last := k.snap.LastPulseAt; last != nil {
		d := p.At - *last
		k.snap.LastInterval = &d
	}
	at := p.At
	k.snap.LastPulseAt = &at
	k.snap.Count++
	k.inPulse, k.pulseAt = true, p.At
	return []Event{{Kind: Pulse, At: p.At, Snap: k.Snapshot()}}
}

func (k *Counter) setLevel(l Level) { k.level, k.snap.Level = l, l }

// Snapshot returns the state now. Its pointers are copies.
func (k *Counter) Snapshot() Snapshot {
	s := k.snap
	for _, p := range []**time.Duration{&s.LastPulseAt, &s.LastInterval, &s.LastClosed} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return s
}
