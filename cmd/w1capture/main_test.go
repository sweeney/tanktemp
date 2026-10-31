package main

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func TestRunCapturesNumberedSteps(t *testing.T) {
	src := fstest.MapFS{"w1_bus_master1/w1_master_slaves": {Data: []byte("not found.\n")}}
	out := filepath.Join(t.TempDir(), "scenario")
	var slept []time.Duration
	err := run([]string{"-out", out, "-steps", "3", "-interval", "2s"}, src, func(d time.Duration) { slept = append(slept, d) })
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"step-001", "step-002", "step-003"} {
		if _, err := os.Stat(filepath.Join(out, step, "w1_bus_master1", "w1_master_slaves")); err != nil {
			t.Errorf("%s: %v", step, err)
		}
	}
	if len(slept) != 2 || slept[0] != 2*time.Second {
		t.Errorf("slept %v, want 2s between 3 steps", slept)
	}
}

func TestRunSingleCaptureHasNoStepDirectory(t *testing.T) {
	src := fstest.MapFS{"w1_bus_master1/w1_master_slaves": {Data: []byte("not found.\n")}}
	out := filepath.Join(t.TempDir(), "scenario")
	if err := run([]string{"-out", out}, src, func(time.Duration) { t.Error("no sleep for one step") }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "w1_bus_master1", "w1_master_slaves")); err != nil {
		t.Error(err)
	}
}

func TestRunNeedsOut(t *testing.T) {
	if err := run(nil, fstest.MapFS{}, func(time.Duration) {}); err == nil {
		t.Error("want an error without -out")
	}
}
