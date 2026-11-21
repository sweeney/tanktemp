package meter_test

import (
	"context"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
)

// gated runs a Loop whose emit blocks on gate for every pulse, so a test can
// hold Run busy while it lines up edges and a tick or request together.
type gated struct {
	src   *metertest.Fake
	ticks chan time.Duration
	gate  chan chan struct{}
	loop  *meter.Loop
}

func startGated(t *testing.T) *gated {
	t.Helper()
	k, err := meter.New(meter.Config{Hold: 20 * ms, MinInterval: 600 * ms}, meter.Open)
	if err != nil {
		t.Fatal(err)
	}
	g := &gated{src: metertest.NewFake(meter.Open), ticks: make(chan time.Duration), gate: make(chan chan struct{}, 1)}
	g.loop = meter.NewLoop(k, g.src, g.ticks, func(e meter.Event) {
		if e.Kind == meter.Pulse {
			select {
			case c := <-g.gate:
				<-c
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); g.loop.Run(ctx) }()
	t.Cleanup(func() { cancel(); g.src.Close(); <-done })
	return g
}

// round makes Run busy (emitting litre i, blocked), then queues a 5 ms
// spike and makes ready, which must not be handled between the spike's two
// edges; then lets Run go. With a Loop that doesn't drain, the spike's
// closing is confirmed in about a quarter of rounds: over 50, a miss is
// under one in a million.
func (g *gated) round(t *testing.T, i int, ready func(now time.Duration)) {
	t.Helper()
	b := time.Duration(i) * 10 * time.Second
	open := make(chan struct{})
	g.gate <- open
	g.src.Push(b, meter.Closed)
	g.src.Push(b+400*ms, meter.Open) // confirms the closing: emit blocks
	for len(g.gate) != 0 {
		time.Sleep(100 * time.Microsecond)
	}
	g.src.Push(b+2000*ms, meter.Closed) // confirms the opening; the spike
	g.src.Push(b+2005*ms, meter.Open)
	go ready(b + 2050*ms)
	time.Sleep(time.Millisecond) // let ready park on its send
	close(open)
}

const rounds = 50

func TestLoopTickNeverOvertakesAQueuedEdgeWhileBusy(t *testing.T) {
	g := startGated(t)
	for i := range rounds {
		g.round(t, i, func(now time.Duration) { g.ticks <- now })
	}
	s, _ := g.loop.Snapshot(context.Background(), rounds*10*time.Second)
	if s.Count != rounds || s.Short != rounds {
		t.Errorf("count %d, short %d; want %d of each (a tick overtook an edge)", s.Count, s.Short, rounds)
	}
}

func TestLoopSnapshotNeverOvertakesAQueuedEdgeWhileBusy(t *testing.T) {
	g := startGated(t)
	for i := range rounds {
		g.round(t, i, func(now time.Duration) { g.loop.Snapshot(context.Background(), now) })
	}
	s, _ := g.loop.Snapshot(context.Background(), rounds*10*time.Second)
	if s.Count != rounds || s.Short != rounds {
		t.Errorf("count %d, short %d; want %d of each (a snapshot overtook an edge)", s.Count, s.Short, rounds)
	}
}

func TestLoopSweepNeverOvertakesAQueuedEdgeWhileBusy(t *testing.T) {
	g := startGated(t)
	for i := range rounds {
		g.round(t, i, func(now time.Duration) { g.loop.Sweep(context.Background(), now) })
	}
	s, _ := g.loop.Snapshot(context.Background(), rounds*10*time.Second)
	if s.Count != rounds || s.Short != rounds {
		t.Errorf("count %d, short %d; want %d of each (a sweep overtook an edge)", s.Count, s.Short, rounds)
	}
}

// A sweep is as of its now (WIRE: "as of the sweep's start"): an edge
// stamped after it, already queued, comes after the sweep's state.
func TestLoopSweepTakesNoEdgeAfterItsNow(t *testing.T) {
	h := start(t, meter.Open)
	h.src.Push(1000*ms, meter.Closed)
	h.src.Push(1400*ms, meter.Open)
	if !h.loop.Sweep(context.Background(), 1010*ms) {
		t.Fatal("Sweep failed")
	}
	a, b := h.next(t), h.next(t)
	if a.ev.Kind != meter.Swept || a.snap.Level != meter.Closed || a.snap.Count != 0 || b.ev.Kind != meter.Pulse {
		t.Errorf("emitted %s (level %v, count %d) then %s; want the sweep as of 1.01 s, then the pulse",
			a.ev.Kind, a.snap.Level, a.snap.Count, b.ev.Kind)
	}
}

// A request already accepted still honours its ctx: a slow emit can't hold
// the caller past its deadline (finish's timeout relies on it).
func TestLoopAskHonoursItsContextOnceAccepted(t *testing.T) {
	g := startGated(t)
	open := make(chan struct{})
	defer close(open)
	g.gate <- open
	g.src.Push(1000*ms, meter.Closed)
	ctx, cancel := context.WithTimeout(context.Background(), 50*ms)
	defer cancel()
	got := make(chan bool, 1)
	go func() { _, ok := g.loop.Snapshot(ctx, 1020*ms); got <- ok }() // confirms the litre: emit blocks
	select {
	case ok := <-got:
		if ok {
			t.Error("Snapshot = true from a loop stuck in emit")
		}
	case <-time.After(500 * ms):
		t.Error("Snapshot still waiting 450 ms past its deadline")
	}
}
