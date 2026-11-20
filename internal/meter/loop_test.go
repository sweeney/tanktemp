package meter_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
)

const ms = time.Millisecond

type emitted struct {
	ev   meter.Event
	snap meter.Snapshot
}

// harness runs a Loop over a Fake, with ticks the test sends.
type harness struct {
	src   *metertest.Fake
	ticks chan time.Duration
	out   chan emitted
	loop  *meter.Loop
	done  chan error
	stop  context.CancelFunc
}

func start(t *testing.T, initial meter.Level) *harness {
	t.Helper()
	k, err := meter.New(meter.Config{Hold: 20 * ms, MinInterval: 600 * ms}, initial)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{src: metertest.NewFake(initial), ticks: make(chan time.Duration), out: make(chan emitted, 100), done: make(chan error, 1)}
	h.loop = meter.NewLoop(k, h.src, h.ticks, func(e meter.Event) { h.out <- emitted{e, e.Snap} })
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go func() { h.done <- h.loop.Run(ctx) }()
	t.Cleanup(func() { cancel(); h.src.Close(); <-h.done })
	return h
}

func (h *harness) next(t *testing.T) emitted {
	t.Helper()
	select {
	case e := <-h.out:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("nothing emitted")
		return emitted{}
	}
}

func (h *harness) none(t *testing.T) {
	t.Helper()
	// A snapshot round trip proves the loop has handled everything sent so
	// far; anything it emitted is then already in out.
	h.loop.Snapshot(context.Background())
	select {
	case e := <-h.out:
		t.Fatalf("emitted %+v", e)
	default:
	}
}

// A held closing is published at the first tick at or after closing + hold:
// so with a tick every 50 ms, within hold + 50 ms (WIRE.md). The pulse is
// stamped at the closing, and the snapshot published with it counts it.
func TestLoopPublishesAPulseAtTheTickAfterItsHold(t *testing.T) {
	h := start(t, meter.Open)
	h.src.Push(1000*ms, meter.Closed)
	h.ticks <- 1010 * ms
	h.none(t)
	h.ticks <- 1050 * ms
	got := h.next(t)
	if got.ev.Kind != meter.Pulse || got.ev.At != 1000*ms || got.snap.Count != 1 || got.snap.Level != meter.Closed {
		t.Errorf("emitted %+v", got)
	}
	h.src.Push(1400*ms, meter.Open)
	got = h.next(t)
	if got.ev.Kind != meter.Opened || got.snap.LastClosed == nil || *got.snap.LastClosed != 400*ms {
		t.Errorf("emitted %+v", got)
	}
}

// An edge confirms a held closing too, with no tick.
func TestLoopEdgeConfirmsWithoutATick(t *testing.T) {
	h := start(t, meter.Open)
	h.src.Push(1000*ms, meter.Closed)
	h.src.Push(1400*ms, meter.Open)
	a, b := h.next(t), h.next(t)
	if a.ev.Kind != meter.Pulse || b.ev.Kind != meter.Opened {
		t.Fatalf("emitted %+v then %+v", a, b)
	}
	// One edge confirmed and ended the litre: the pulse's snapshot is the
	// state at the pulse (closed, no closed time yet), not after the open.
	if a.snap.Level != meter.Closed || a.snap.LastClosed != nil || b.snap.Level != meter.Open || *b.snap.LastClosed != 400*ms {
		t.Errorf("pulse snapshot %+v, open snapshot %+v", a.snap, b.snap)
	}
}

// Snapshot reflects every edge sent before it was asked for.
func TestLoopSnapshot(t *testing.T) {
	h := start(t, meter.Closed)
	h.src.Push(1000*ms, meter.Open)
	s, ok := h.loop.Snapshot(context.Background())
	if !ok || s.Level != meter.Open || s.Count != 0 {
		t.Errorf("snapshot %+v, %v", s, ok)
	}
}

// Run stops when its context ends, and a Snapshot asked for after that
// reports it couldn't be had rather than hang.
func TestLoopStopsWithItsContext(t *testing.T) {
	h := start(t, meter.Open)
	h.stop()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run = %v, want nil on cancel", err)
		}
		h.done <- nil // for the cleanup
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*ms)
	defer cancel()
	if _, ok := h.loop.Snapshot(ctx); ok {
		t.Error("a snapshot from a stopped loop")
	}
}

// If the source closes under it (the line was lost), Run says so.
func TestLoopReportsAClosedSource(t *testing.T) {
	h := start(t, meter.Open)
	h.src.Close()
	select {
	case err := <-h.done:
		if err == nil {
			t.Error("Run = nil, want an error for a closed source")
		}
		h.done <- nil
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

// Snapshots from many goroutines while edges and ticks stream: run with
// -race, the loop is the only goroutine touching the Counter.
func TestLoopConcurrentSnapshots(t *testing.T) {
	h := start(t, meter.Open)
	go func() {
		for range h.out {
		}
	}()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				h.loop.Snapshot(context.Background())
			}
		}()
	}
	for i := range 400 {
		at := time.Duration(i) * 700 * ms
		l := meter.Closed
		if i%2 == 1 {
			l = meter.Open
		}
		h.src.Push(at, l)
		if i%10 == 0 {
			h.ticks <- at + 30*ms
		}
	}
	wg.Wait()
	if s, _ := h.loop.Snapshot(context.Background()); s.Count != 200 {
		t.Errorf("count %d, want 200", s.Count)
	}
}
