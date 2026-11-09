package main

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/common/mqttpub/mqttpubtest"
	"github.com/sweeney/tanktemp/internal/cadence"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// Hand-written fakes for sweepJob's three small interfaces (CLAUDE.md: no
// mockgen). Each records what it was asked.

type fakeSweeper struct {
	sw  sweep.Sweep
	err error
}

func (f *fakeSweeper) Sweep(context.Context) (sweep.Sweep, error) { return f.sw, f.err }

type fakeCadence struct {
	decision cadence.Decision
	status   cadence.Status
	observed []time.Time // the started time of each Observe
}

func (f *fakeCadence) Observe(_ sweep.Sweep, started time.Time) cadence.Decision {
	f.observed = append(f.observed, started)
	return f.decision
}
func (f *fakeCadence) Status() cadence.Status { return f.status }

type fakeRecorder struct {
	sweeps   int
	cadences []cadence.Status
}

func (f *fakeRecorder) RecordSweep(sweep.Sweep)        { f.sweeps++ }
func (f *fakeRecorder) RecordCadence(c cadence.Status) { f.cadences = append(f.cadences, c) }

// jobSweep is one tank of one probe, read OK, or failed with a CRC error.
func jobSweep(failed bool) sweep.Sweep {
	v := 40.0
	s := sweep.Sensor{Position: "bottom", Order: 1, ROM: bottom, Code: reading.OK, RawC: &v, TempC: &v, Attempts: 1}
	if failed {
		s = sweep.Sensor{Position: "bottom", Order: 1, ROM: bottom, Code: reading.CRC, Attempts: 3}
	}
	return sweep.Sweep{At: t0, ClockSynced: true, Tanks: []sweep.Tank{{DeviceID: "cyl", Sensors: []sweep.Sensor{s}}}}
}

// sweepJob.Work is one unit of lifecycle work: sweep, observe, record,
// publish, log. Every path is pinned here, without booting run.
func TestSweepJob(t *testing.T) {
	boom := errors.New("broker gone")
	fastTrig := &cadence.Trigger{DeviceID: "cyl", Position: "bottom", CPerMin: -8}
	tests := []struct {
		name      string
		failed    bool // the probe fails (CRC), so there's a fault line
		sweepErr  error
		pubErr    error
		decision  cadence.Decision
		wantErr   error
		observed  int           // Observe calls
		recorded  int           // RecordSweep and RecordCadence calls
		published int           // state messages
		interval  time.Duration // what Interval says afterwards
		verbose   string        // in the verbose log
		normal    string        // in the normal log
		// both logs, interleaved as written ("V " verbose, "N " normal):
		// Work promises sweep line, fault line, cadence line, in that order
		order string
	}{
		{
			name:     "a quiet sweep: slow, nothing verbose but the sweep line",
			decision: cadence.Decision{Mode: cadence.Slow, Interval: 30 * time.Second},
			observed: 1, recorded: 1, published: 1, interval: 30 * time.Second,
			verbose: "sweep 0s 1/1 cyl 40.0\n",
		},
		{
			name:     "a trigger: fast, and the cadence line after the sweep line",
			decision: cadence.Decision{Mode: cadence.Hold, Interval: 10 * time.Second, Trigger: fastTrig, Changed: true, AtFast: true},
			observed: 1, recorded: 1, published: 1, interval: 10 * time.Second,
			verbose: "sweep 0s 1/1 cyl 40.0\ncadence: fast 10s (cyl/bottom -8.0 °C/min)\n",
		},
		{
			name:     "a failed probe and a trigger: the fault line, between the sweep line and the cadence line",
			failed:   true,
			decision: cadence.Decision{Mode: cadence.Hold, Interval: 10 * time.Second, Trigger: fastTrig, Changed: true, AtFast: true},
			observed: 1, recorded: 1, published: 1, interval: 10 * time.Second,
			verbose: "sweep 0s 0/1 cyl null\ncadence: fast 10s (cyl/bottom -8.0 °C/min)\n",
			normal:  "sweep: 0 ok, 1 failed, 0s: crc: cyl/bottom\n",
			order:   "V sweep 0s 0/1 cyl null\nN sweep: 0 ok, 1 failed, 0s: crc: cyl/bottom\nV cadence: fast 10s (cyl/bottom -8.0 °C/min)\n",
		},
		{
			name:     "a failed sweep (cancelled at shutdown): nothing observed, recorded or published; the interval stays",
			sweepErr: sweep.ErrCancelled, wantErr: sweep.ErrCancelled,
			observed: 0, recorded: 0, published: 0, interval: 45 * time.Second,
		},
		{
			name:   "a publish error: still observed and recorded, and the interval moves on",
			pubErr: boom, wantErr: boom,
			decision: cadence.Decision{Mode: cadence.Decay, Interval: 14 * time.Second},
			observed: 1, recorded: 1, published: 0, interval: 14 * time.Second,
			verbose: "sweep 0s 1/1 cyl 40.0\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var normal, verbose, both strings.Builder
			cad := &fakeCadence{decision: tt.decision, status: cadence.Status{Mode: tt.decision.Mode}}
			rec := &fakeRecorder{}
			pub := &mqttpubtest.Fake{PublishError: tt.pubErr}
			j := &sweepJob{
				sweeper: &fakeSweeper{sw: jobSweep(tt.failed), err: tt.sweepErr},
				cadence: cad, tracker: rec, pub: pub, prefix: prefix,
				logger:   log.New(io.MultiWriter(&normal, prefixed(&both, "N ")), "", 0),
				verbose:  log.New(io.MultiWriter(&verbose, prefixed(&both, "V ")), "", 0),
				interval: 45 * time.Second, // as if decided by an earlier sweep
			}
			started := t0.Add(7 * time.Second)
			err := j.Work(context.Background(), started)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if len(cad.observed) != tt.observed || (tt.observed > 0 && !cad.observed[0].Equal(started)) {
				t.Errorf("Observe calls %v, want %d with lifecycle's start %v", cad.observed, tt.observed, started)
			}
			if rec.sweeps != tt.recorded || len(rec.cadences) != tt.recorded {
				t.Errorf("recorded %d sweeps and %d cadences, want %d of each", rec.sweeps, len(rec.cadences), tt.recorded)
			}
			msgs := pub.Snapshot()
			if len(msgs) != tt.published {
				t.Errorf("published %d, want %d", len(msgs), tt.published)
			}
			for _, m := range msgs { // the wire shape run had before the extraction
				if m.Topic != prefix+"/cyl/state" || !m.Retained || m.QOS != 0 {
					t.Errorf("published %s retained=%v qos=%d, want %s/cyl/state, retained, QoS 0", m.Topic, m.Retained, m.QOS, prefix)
				}
			}
			if got := j.Interval(); got != tt.interval {
				t.Errorf("Interval() = %v, want %v", got, tt.interval)
			}
			if tt.verbose != "" && verbose.String() != tt.verbose {
				t.Errorf("verbose log %q, want %q", verbose.String(), tt.verbose)
			}
			if tt.verbose == "" && verbose.String() != "" {
				t.Errorf("verbose log %q, want nothing", verbose.String())
			}
			if normal.String() != tt.normal {
				t.Errorf("normal log %q, want %q", normal.String(), tt.normal)
			}
			if tt.order != "" && both.String() != tt.order {
				t.Errorf("both logs, in order:\n%s\nwant:\n%s", both.String(), tt.order)
			}
		})
	}
}

// nextSweep is lifecycle's Config.Next: each due time from the Schedule,
// with the interval the last sweep decided, told to the tracker, and an
// overrun's skipped ticks counted there.
func TestNextSweep(t *testing.T) {
	now := t0
	interval := 30 * time.Second
	var dues []time.Time
	skips, calls := 0, 0
	next := nextSweep(&cadence.Schedule{}, func() time.Time { return now }, func() time.Duration { return interval },
		func(due time.Time) { dues = append(dues, due) }, func(n int) { calls++; skips += n })

	now = t0.Add(4 * time.Second)
	if due := next(t0, nil); !due.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("first: +%v, want +30s", due.Sub(t0))
	}
	interval = 10 * time.Second
	now = t0.Add(55 * time.Second) // that sweep overran: +40 and +50 passed
	if due := next(t0.Add(30*time.Second), nil); !due.Equal(t0.Add(60 * time.Second)) {
		t.Errorf("after an overrun: +%v, want +60s", due.Sub(t0))
	}
	if len(dues) != 2 || !dues[1].Equal(t0.Add(60*time.Second)) || skips != 2 {
		t.Errorf("tracker told due %v and %d skips; want 2 dues ending +60s, 2 skips", dues, skips)
	}
	// One call with the count, however long the stall (review on #4).
	if calls != 1 {
		t.Errorf("onSkip called %d times, want once, with the count", calls)
	}
}

// prefixed writes each write to w with p in front: log.Logger writes one
// line per call, so it tags each line with the logger it came from.
func prefixed(w io.Writer, p string) io.Writer { return prefixWriter{w, p} }

type prefixWriter struct {
	w io.Writer
	p string
}

func (pw prefixWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(pw.w, pw.p); err != nil {
		return 0, err
	}
	return pw.w.Write(b)
}
