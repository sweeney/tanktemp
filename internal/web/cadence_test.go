package web

import (
	"strings"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/cadence"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/status"
)

// Issue #1: /api/status carries a sweep block, computed per request from
// the tracker, for tuning the cadence. Times are absolute (next_due, not
// "in N s"), so a value stays true for as long as it's on screen.
func TestAPIStatusSweepBlock(t *testing.T) {
	s := server(true, sweepAt(reading.OK))
	trig := &cadence.Trigger{At: start.Add(30 * time.Second), DeviceID: "cyl", Position: "bottom", ROM: "28-000011721ce9", CPerMin: -3.2049, DeltaC: -1.6}
	s.Tracker.RecordCadence(cadence.Status{Enabled: true, Mode: cadence.Decay, Interval: 14207 * time.Millisecond, LastTrigger: trig, FastEntries: 17})
	s.Tracker.SetNextDue(start.Add(42*time.Second + 600*time.Millisecond))

	_, body := get(t, s, "/api/status")
	sw, ok := body["sweep"].(map[string]any)
	if !ok {
		t.Fatalf("no sweep block: %v", body)
	}
	if sw["adaptive"] != true || sw["mode"] != "decay" || sw["interval_s"] != 14.207 || sw["fast_entries"] != float64(17) {
		t.Errorf("sweep = %v", sw)
	}
	// RFC 3339 to the second, like every other time tanktemp reports.
	if sw["next_due"] != "2026-11-02T21:00:42Z" {
		t.Errorf("next_due = %v", sw["next_due"])
	}
	lt, ok := sw["last_trigger"].(map[string]any)
	if !ok || lt["at"] != "2026-11-02T21:00:30Z" || lt["probe"] != "cyl.bottom" || lt["rom"] != "28-000011721ce9" || lt["c_per_min"] != -3.2 {
		t.Errorf("last_trigger = %v; want the statehouse primary as probe, and the rate to 0.01", sw["last_trigger"])
	}
}

// A config without sweep.fast reports mode slow at the interval, and no
// trigger. Before the scheduler has said, next_due is null: unknown, not 0.
func TestAPIStatusSweepBlockWithoutFast(t *testing.T) {
	s := server(true, sweepAt(reading.OK))
	s.Tracker = status.New(status.Options{Start: start, Interval: 30 * time.Second,
		Cadence: cadence.New(cadence.Settings{Slow: 30 * time.Second}).Status()})
	_, body := get(t, s, "/api/status")
	sw := body["sweep"].(map[string]any)
	if sw["adaptive"] != false || sw["mode"] != "slow" || sw["interval_s"] != float64(30) || sw["fast_entries"] != float64(0) {
		t.Errorf("sweep = %v", sw)
	}
	for _, k := range []string{"next_due", "last_trigger"} {
		if v, present := sw[k]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, present)
		}
	}
}

// /healthz is unchanged by #1: no sweep block there.
func TestHealthzHasNoSweepBlock(t *testing.T) {
	_, body := get(t, server(true, sweepAt(reading.OK)), "/healthz")
	if _, present := body["sweep"]; present {
		t.Errorf("/healthz has a sweep block: %v", body["sweep"])
	}
}

// The page counts down to next_due and shows the mode.
func TestPageShowsTheCadence(t *testing.T) {
	rec, _ := get(t, server(true, sweepAt(reading.OK)), "/")
	for _, want := range []string{"cadence = s.sweep", "cadence.next_due", "cadence.mode", `id="cadence"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
