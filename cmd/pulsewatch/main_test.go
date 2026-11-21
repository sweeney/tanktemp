package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
)

const ms = time.Millisecond

// watch prints each edge with what the counter made of it, then a summary.
// One run with every verdict, the reed starting open:
//
//	seq 1  1000 ms closed  held
//	seq 2  1400 ms open    confirms litre 1 (at seq 1); the opening is held
//	seq 3  1500 ms open    noise: already open; confirms the opening: closed 400 ms
//	seq 4  2000 ms closed  held
//	seq 5  2005 ms open    short: reopened in 5 ms, under the 20 ms hold
//	seq 6  2100 ms closed  held (1100 ms after litre 1)
//	seq 7  2500 ms open    litre 2
//	seq 8  2600 ms closed  held; litre 2 ended, closed 400 ms
//	seq 9  2700 ms open    glitch: litre 3 would be 500 ms after litre 2
//	seq 13 3600 ms open    3 edges lost before it
func TestWatch(t *testing.T) {
	src := metertest.NewFake(meter.Open)
	for _, e := range []struct {
		at time.Duration
		l  meter.Level
	}{
		{1000, meter.Closed}, {1400, meter.Open}, {1500, meter.Open},
		{2000, meter.Closed}, {2005, meter.Open},
		{2100, meter.Closed}, {2500, meter.Open},
		{2600, meter.Closed}, {2700, meter.Open},
	} {
		src.Push(e.at*ms, e.l)
	}
	src.Send(meter.Edge{At: 3600 * ms, Level: meter.Open, Seqno: 13})
	src.Close()
	k, _ := meter.New(meter.Config{Hold: 20 * ms, MinInterval: 600 * ms}, meter.Open)
	var out strings.Builder
	watch(context.Background(), src, nil, k, &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := [][]string{
		{"seq 1 ", "closed", "+0.000s"},
		{"seq 2 ", "open", "+0.400s", "litre 1"},
		{"seq 3 ", "open", "noise", "litre 1 ended: closed 400ms"},
		{"seq 4 ", "closed"},
		{"seq 5 ", "open", "short"},
		{"seq 6 ", "closed"},
		{"seq 7 ", "open", "litre 2"},
		{"seq 8 ", "closed", "litre 2 ended: closed 400ms"},
		{"seq 9 ", "open", "glitch"},
		{"seq 13 ", "open", "3 lost before it"},
		{"2 litres", "noise 1", "short 1", "glitches 1", "lost 3"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), len(want), out.String())
	}
	for i, w := range want {
		for _, s := range w {
			if !strings.Contains(lines[i], s) {
				t.Errorf("line %d %q: no %q", i, lines[i], s)
			}
		}
	}
	if strings.Contains(lines[4], "litre") || strings.Contains(lines[8], "litre 3") {
		t.Errorf("a short or a glitch counted:\n%s", out.String())
	}
}

// A tick confirms a closing that rests on the magnet, and says so.
func TestWatchTickConfirms(t *testing.T) {
	src := metertest.NewFake(meter.Open)
	ticks := make(chan time.Duration, 1)
	k, _ := meter.New(meter.Config{Hold: 20 * ms, MinInterval: 600 * ms}, meter.Open)
	var out strings.Builder
	done := make(chan struct{})
	go func() { defer close(done); watch(context.Background(), src, ticks, k, &out) }()
	src.Push(1000*ms, meter.Closed)
	time.Sleep(10 * ms) // let watch take the edge before the tick
	ticks <- 1050 * ms
	time.Sleep(10 * ms)
	src.Close()
	<-done
	if !strings.Contains(out.String(), "litre 1 (held 50ms, resting on the magnet)") {
		t.Errorf("out:\n%s", out.String())
	}
}
