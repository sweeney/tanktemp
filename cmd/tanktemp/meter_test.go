package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
	"github.com/sweeney/tanktemp/internal/payload"
)

const ms = time.Millisecond

// A fixed pair of clocks: wall 2026-11-20 07:05:10.500 is monotonic 10.5 s.
var (
	wallAt = time.Date(2026, 11, 20, 7, 5, 10, 500e6, time.UTC)
	monoAt = 10500 * ms
)

func testMeterCfg() config.Meter {
	m := config.DefaultMeter
	gpio := 27
	m.DeviceID, m.GPIO, m.LitresPerPulse = "hot_water_meter", &gpio, 1
	return m
}

func newTestMeter(t *testing.T, pub publisher) (*meterRun, *metertest.Fake) {
	t.Helper()
	src := metertest.NewFake(meter.Open)
	m, err := newMeter(testMeterCfg(), src, meterDeps{
		Pub: pub, Prefix: "water/hot/tanktemp",
		Wall: func() time.Time { return wallAt }, Mono: func() time.Duration { return monoAt },
		Synced: func() bool { return true }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); src.Close(); <-done })
	return m, src
}

// published waits for n messages on the meter topic: publishing is
// asynchronous, on its own goroutine.
func published(t *testing.T, pub *mqttpubtest.Fake, n int) []mqttpub.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		all := pub.All("water/hot/tanktemp/meter")
		if len(all) >= n || time.Now().After(deadline) {
			if len(all) != n {
				t.Fatalf("%d meter messages, want %d: %+v", len(all), n, all)
			}
			return all
		}
		time.Sleep(time.Millisecond)
	}
}

// A counted litre is published at once on {prefix}/meter, retained, with
// the event's monotonic time turned into wall time: the closing at mono
// 4.007 s is 6.493 s before "now", so 07:05:04.007.
func TestMeterPublishesAPulse(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	m, _ := newTestMeter(t, pub)
	m.emit(meter.Event{Kind: meter.Pulse, At: 4007 * ms, Snap: meter.Snapshot{
		Count: 1, Level: meter.Closed, LastPulseAt: ptrDur(4007 * ms),
	}})
	msg := published(t, pub, 1)[0]
	want := `{"device_id":"hot_water_meter","timestamp":"2026-11-20T07:05:04Z","clock_synced":true,"trigger":"pulse","seq":1,"litres_per_pulse":1,"level":"closed","counting_since":"2026-11-20T07:05:10Z","count":1,"last_pulse_at":"2026-11-20T07:05:04.007Z","last_interval_ms":null,"last_closed_ms":null,"noise":0,"short":0,"glitches":0,"lost":0}`
	if msg.Payload != want || msg.QOS != 0 || !msg.Retained {
		t.Errorf("published %+v\nwant payload %s", msg, want)
	}
}

// The opening that ends a litre is "open", numbered on.
func TestMeterPublishesAnOpen(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	m, _ := newTestMeter(t, pub)
	m.emit(meter.Event{Kind: meter.Pulse, At: 4007 * ms, Snap: meter.Snapshot{Count: 1, Level: meter.Closed, LastPulseAt: ptrDur(4007 * ms)}})
	m.emit(meter.Event{Kind: meter.Opened, At: 5548 * ms, Snap: meter.Snapshot{Count: 1, Level: meter.Open, LastPulseAt: ptrDur(4007 * ms), LastClosed: ptrDur(1541 * ms)}})
	all := published(t, pub, 2)
	if !contains(all[1].Payload, `"timestamp":"2026-11-20T07:05:05Z"`, `"trigger":"open","seq":2`, `"last_closed_ms":1541`) {
		t.Errorf("published %+v", all)
	}
}

// A sweep's meter message is the meter as of the sweep's start, stamped
// with the sweep's timestamp, and numbered in the same series as pulses.
func TestMeterPublishesWithASweep(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	m, src := newTestMeter(t, pub)
	src.Push(10490*ms, meter.Closed) // held 10 ms by "now": not yet a litre
	sweepAt := time.Date(2026, 11, 20, 7, 6, 0, 0, time.UTC)
	m.sweep(context.Background(), sweepAt)
	msg := published(t, pub, 1)[0]
	if !contains(msg.Payload, `"timestamp":"2026-11-20T07:06:00Z"`, `"trigger":"sweep","seq":1`, `"level":"closed"`, `"count":0`) {
		t.Errorf("published %+v", msg)
	}
}

// Pulses (the loop's goroutine) and sweeps (lifecycle's) publish at once:
// every message gets its own seq.
func TestMeterSeqIsUniqueAcrossGoroutines(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	m, _ := newTestMeter(t, pub)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.emit(meter.Event{Kind: meter.Pulse, At: time.Duration(i) * ms, Snap: meter.Snapshot{Level: meter.Closed}})
		}()
		go func() { defer wg.Done(); m.publish(payload.TriggerSweep, wallAt, meter.Snapshot{Level: meter.Open}) }()
	}
	wg.Wait()
	// Sweep messages may coalesce (a later one replaces one still queued);
	// every pulse goes, and no seq repeats.
	deadline := time.Now().Add(2 * time.Second)
	for m.queued() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * ms) // the last one taken may still be publishing
	seen, pulses := map[string]bool{}, 0
	for _, msg := range pub.All("water/hot/tanktemp/meter") {
		s := seqOf(t, msg.Payload)
		if seen[s] {
			t.Fatalf("seq %s twice", s)
		}
		seen[s] = true
		if strings.Contains(msg.Payload, `"trigger":"pulse"`) {
			pulses++
		}
	}
	if pulses != 20 {
		t.Errorf("%d pulse messages, want 20", pulses)
	}
}

func ptrDur(d time.Duration) *time.Duration { return &d }

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

func seqOf(t *testing.T, payload string) string {
	t.Helper()
	_, rest, ok := strings.Cut(payload, `"seq":`)
	n, _, ok2 := strings.Cut(rest, ",")
	if !ok || !ok2 {
		t.Fatalf("no seq in %s", payload)
	}
	return n
}

// blockingPub holds every Publish until released: a broker gone slow.
type blockingPub struct {
	release chan struct{}
	mqttpubtest.Fake
}

func (b *blockingPub) Publish(m mqttpub.Message) error {
	<-b.release
	return b.Fake.Publish(m)
}

// gatePub holds every Publish until released, and records whether each
// came after done was closed.
type gatePub struct {
	mqttpubtest.Fake
	release, done chan struct{}
	mu            sync.Mutex
	late          int
}

func (g *gatePub) Publish(m mqttpub.Message) error {
	<-g.release
	select {
	case <-g.done:
		g.mu.Lock()
		g.late++
		g.mu.Unlock()
	default:
	}
	return g.Fake.Publish(m)
}

// Run doesn't return while its publisher can still publish (review on #9, a
// blocker): mqttpub's Close picks a clean disconnect or the last will from
// the last Publish, from any goroutine, so a meter message after Run (and
// so after SHUTDOWN) could flip it. The publisher used to drain its whole
// queue after Run had returned.
func TestMeterRunOutlivesItsPublisher(t *testing.T) {
	pub := &gatePub{release: make(chan struct{}), done: make(chan struct{})}
	src := metertest.NewFake(meter.Open)
	m, err := newMeter(testMeterCfg(), src, meterDeps{
		Pub: pub, Prefix: "water/hot/tanktemp",
		Wall: func() time.Time { return wallAt }, Mono: func() time.Duration { return monoAt },
		Synced: func() bool { return true }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); m.Run(ctx) }()
	for range 5 {
		m.publish(payload.TriggerSweep+"x", wallAt, meter.Snapshot{Level: meter.Open}) // not coalesced
	}
	waitQueued(t, m, 4) // one is in the gated Publish
	cancel()
	go func() { time.Sleep(20 * time.Millisecond); close(pub.release) }()
	<-ran
	close(pub.done)
	time.Sleep(150 * time.Millisecond) // the gate is open: anything left would publish now
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.late != 0 {
		t.Errorf("%d meter messages published after Run returned", pub.late)
	}
}

// A stalled broker must not stall the counting (review on 1a037c4): with
// every publish blocked, edges keep being counted, and once it frees up
// the messages go, in order.
func TestMeterCountsWhilePublishingStalls(t *testing.T) {
	pub := &blockingPub{release: make(chan struct{})}
	m, src := newTestMeter(t, pub)
	for i := range 10 {
		src.Push(time.Duration(i)*time.Second, meter.Closed)
		src.Push(time.Duration(i)*time.Second+400*ms, meter.Open)
	}
	s, ok := m.loop.Snapshot(context.Background(), 20*time.Second)
	if !ok || s.Count != 10 {
		t.Fatalf("with the broker stalled: snapshot %+v, %v; want 10 counted", s, ok)
	}
	close(pub.release)
	all := published(t, &pub.Fake, 20)
	if seqOf(t, all[0].Payload) != "1" || seqOf(t, all[19].Payload) != "20" {
		t.Errorf("messages out of order: first seq %s, last %s", seqOf(t, all[0].Payload), seqOf(t, all[19].Payload))
	}
}

// The queue is bounded (a broker outage mustn't grow memory on a Pi Zero):
// past meterQueue it drops the OLDEST litre messages and counts them. The
// litres aren't lost (each message carries the cumulative count); only
// some litres' own messages are. What survives keeps its order.
func TestMeterQueueIsBoundedAndDropsTheOldest(t *testing.T) {
	pub := &blockingPub{release: make(chan struct{})}
	m, src := newTestMeter(t, pub)
	for i := range 1000 { // 2,000 edges, 2,000 messages, in chunks the Fake can hold
		src.Push(time.Duration(i)*time.Second, meter.Closed)
		src.Push(time.Duration(i)*time.Second+400*ms, meter.Open)
		if i%200 == 199 {
			m.loop.Snapshot(context.Background(), time.Duration(i+1)*time.Second)
		}
	}
	s, _ := m.loop.Snapshot(context.Background(), 1001*time.Second)
	if s.Count != 1000 || s.Lost != 0 {
		t.Fatalf("snapshot %+v; want 1000 counted, none lost", s)
	}
	// One message is in the stalled Publish; the queue holds meterQueue.
	if q, d := m.queued(), m.dropped(); q != meterQueue || d != 2000-1-meterQueue {
		t.Errorf("queued %d, dropped %d; want %d and %d", q, d, meterQueue, 2000-1-meterQueue)
	}
	close(pub.release)
	all := published(t, &pub.Fake, 1+meterQueue)
	// Litre i's messages were the (2i-1)th and 2ith; the oldest 975 went,
	// so after the first (litre 1's pulse) come messages 977 (litre 489's
	// pulse) to 2000 (litre 1000's open), in order, numbered 1, 2, 3...
	for i, p := range all {
		if seqOf(t, p.Payload) != strconv.Itoa(i+1) {
			t.Fatalf("message %d has seq %s", i, seqOf(t, p.Payload))
		}
	}
	if !contains(all[0].Payload, `"trigger":"pulse"`, `"count":1,`) || !contains(all[1].Payload, `"trigger":"pulse"`, `"count":489,`) ||
		!contains(all[meterQueue].Payload, `"trigger":"open"`, `"count":1000,`) {
		t.Errorf("survivors %s / %s / %s", all[0].Payload, all[1].Payload, all[meterQueue].Payload)
	}
}

// A sweep message replaces any sweep message still queued: only the latest
// sweep state matters, and every litre message still goes.
func TestMeterSweepMessagesCoalesce(t *testing.T) {
	pub := &blockingPub{release: make(chan struct{})}
	m, _ := newTestMeter(t, pub)
	m.publish(payload.TriggerSweep, wallAt, meter.Snapshot{Level: meter.Open}) // taken: stalls in Publish
	waitQueued(t, m, 0)
	m.publish(payload.TriggerSweep, wallAt, meter.Snapshot{Level: meter.Open})
	m.emit(meter.Event{Kind: meter.Pulse, Snap: meter.Snapshot{Level: meter.Closed, Count: 1}})
	m.publish(payload.TriggerSweep, wallAt, meter.Snapshot{Level: meter.Closed, Count: 1})
	if q := m.queued(); q != 2 {
		t.Errorf("queued %d, want 2: the pulse and the latest sweep", q)
	}
	close(pub.release)
	all := published(t, &pub.Fake, 3)
	if !contains(all[1].Payload, `"trigger":"pulse"`) || !contains(all[2].Payload, `"trigger":"sweep","seq":3`) {
		t.Errorf("published %v", all)
	}
}

func waitQueued(t *testing.T, m *meterRun, n int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); m.queued() != n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("queued %d, want %d", m.queued(), n)
		}
	}
}

// In seq order the published count never goes backwards (review on
// 376ae58): sweeps snapshot on one goroutine while litres are counted on
// another, and their messages must still be numbered in the order the
// counter produced them.
func TestMeterCountNeverGoesBackwardsInSeqOrder(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	m, src := newTestMeter(t, pub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 300 {
			src.Push(time.Duration(i)*time.Second, meter.Closed)
			src.Push(time.Duration(i)*time.Second+400*ms, meter.Open)
		}
	}()
	for range 300 {
		m.sweep(context.Background(), wallAt)
	}
	<-done
	m.sweep(context.Background(), wallAt)
	deadline := time.Now().Add(2 * time.Second)
	for m.queued() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * ms)
	type msg struct{ seq, count int }
	var got []msg
	for _, p := range pub.All("water/hot/tanktemp/meter") {
		s, _ := strconv.Atoi(seqOf(t, p.Payload))
		_, rest, _ := strings.Cut(p.Payload, `"count":`)
		c, _, _ := strings.Cut(rest, ",")
		n, _ := strconv.Atoi(c)
		got = append(got, msg{s, n})
	}
	slices.SortFunc(got, func(a, b msg) int { return a.seq - b.seq })
	for i := 1; i < len(got); i++ {
		if got[i].count < got[i-1].count {
			t.Fatalf("seq %d count %d after seq %d count %d", got[i].seq, got[i].count, got[i-1].seq, got[i-1].count)
		}
	}
}

// Numbered as they're published: seq has no gaps, even when sweep messages
// coalesce or the queue drops one (review on 376ae58).
func TestMeterSeqHasNoGaps(t *testing.T) {
	pub := &blockingPub{release: make(chan struct{})}
	m, _ := newTestMeter(t, pub)
	m.emit(meter.Event{Kind: meter.Pulse, Snap: meter.Snapshot{Level: meter.Closed, Count: 1}})
	waitQueued(t, m, 0) // taken: stalls in Publish
	m.sweep(context.Background(), wallAt)
	m.sweep(context.Background(), wallAt) // replaces the first
	m.emit(meter.Event{Kind: meter.Opened, Snap: meter.Snapshot{Level: meter.Open, Count: 1}})
	close(pub.release)
	all := published(t, &pub.Fake, 3)
	for i, p := range all {
		if seqOf(t, p.Payload) != strconv.Itoa(i+1) {
			t.Errorf("message %d has seq %s", i, seqOf(t, p.Payload))
		}
	}
}

// One message's times come from one reading of the clocks (review on #9):
// with a wall clock that moves a second on every read, a pulse's timestamp
// and last_pulse_at still name the same instant.
func TestMeterMessageTimesAgree(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	src := metertest.NewFake(meter.Open)
	var mu sync.Mutex
	w := wallAt
	m, err := newMeter(testMeterCfg(), src, meterDeps{
		Pub: pub, Prefix: "water/hot/tanktemp",
		Wall: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			w = w.Add(time.Second)
			return w
		},
		Mono: func() time.Duration { return monoAt }, Synced: func() bool { return true }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	m.emit(meter.Event{Kind: meter.Pulse, At: monoAt, Snap: meter.Snapshot{Level: meter.Closed, Count: 1, LastPulseAt: ptrDur(monoAt)}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	p := published(t, pub, 1)[0].Payload
	var got struct {
		Timestamp   string `json:"timestamp"`
		LastPulseAt string `json:"last_pulse_at"`
	}
	if err := json.Unmarshal([]byte(p), &got); err != nil {
		t.Fatal(err)
	}
	if got.LastPulseAt[:19] != got.Timestamp[:19] {
		t.Errorf("timestamp %s, last_pulse_at %s: one pulse, two instants", got.Timestamp, got.LastPulseAt)
	}
}

// A sweep message is stamped with the sweep's start, unless the loop took
// an edge stamped after it into that state; then it moves on to that edge,
// so the state never includes a litre after its timestamp (review on #9).
func TestMeterSweepIsStampedNoEarlierThanItsLatestEdge(t *testing.T) {
	pub := &mqttpubtest.Fake{}
	m, _ := newTestMeter(t, pub)
	m.sweepAt, m.sweepMono = wallAt, monoAt
	m.emit(meter.Event{Kind: meter.Swept, At: monoAt, Snap: meter.Snapshot{Level: meter.Open}})
	published(t, pub, 1) // one at a time: a waiting sweep message is replaced by the next
	m.emit(meter.Event{Kind: meter.Swept, At: monoAt + 3*time.Second, Snap: meter.Snapshot{Level: meter.Open}})
	all := published(t, pub, 2)
	want0, want1 := `"timestamp":"`+wallAt.UTC().Format(time.RFC3339)+`"`, `"timestamp":"`+wallAt.Add(3*time.Second).UTC().Format(time.RFC3339)+`"`
	if !strings.Contains(all[0].Payload, want0) || !strings.Contains(all[1].Payload, want1) {
		t.Errorf("published\n%s\n%s\nwant %s, then %s", all[0].Payload, all[1].Payload, want0, want1)
	}
}
