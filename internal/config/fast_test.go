package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Issue #1: sweep.fast is the adaptive cadence. These tests pin how it is
// read and refused. What it *does* is internal/cadence's business.

// fastValid is valid plus a complete sweep.fast block, as the issue writes it.
const fastValid = valid + `
sweep:
  interval: 30s
  attempts: 3
  fast:
    interval: 10s
    trigger_c_per_min: 1.5
    hold: 2m
    decay: 5m
`

// With no fast block the cadence is fixed, as it was before #1: Fast is
// nil, and nothing else about the config moves.
func TestFastAbsentIsNil(t *testing.T) {
	c, err := Load([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sweep.Fast != nil {
		t.Errorf("no sweep.fast in the file, but Fast = %+v", *c.Sweep.Fast)
	}
	if c.Sweep.Interval != 30*time.Second || c.Sweep.Attempts != 3 {
		t.Errorf("sweep = %+v, want the defaults unchanged", c.Sweep)
	}
}

// fast: null (or a bare "fast:") is the same as leaving it out. YAML can't
// tell "fast:" from "fast: null", and neither says "on".
func TestFastNullIsAbsent(t *testing.T) {
	c, err := Load([]byte(valid + "sweep:\n  fast:\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sweep.Fast != nil {
		t.Errorf("fast: (null) gave Fast = %+v, want nil", *c.Sweep.Fast)
	}
}

func TestFastParses(t *testing.T) {
	c, err := Load([]byte(fastValid))
	if err != nil {
		t.Fatal(err)
	}
	want := Fast{Interval: 10 * time.Second, TriggerCPerMin: 1.5, Hold: 2 * time.Minute, Decay: 5 * time.Minute}
	if c.Sweep.Fast == nil || *c.Sweep.Fast != want {
		t.Errorf("Fast = %+v, want %+v", c.Sweep.Fast, want)
	}
}

// A key left out of the block takes its default, so "fast: {}" turns the
// adaptive cadence on with the issue's numbers. An explicit value, even a
// zero, is kept: "hold: 0s" means "start easing back at once", not "use
// the default hold".
func TestFastDefaultsPerKey(t *testing.T) {
	c, err := Load([]byte(valid + "sweep:\n  fast: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sweep.Fast == nil || *c.Sweep.Fast != DefaultFast {
		t.Errorf("fast: {} gave %+v, want DefaultFast %+v", c.Sweep.Fast, DefaultFast)
	}
	if DefaultFast != (Fast{Interval: 10 * time.Second, TriggerCPerMin: 1.5, Hold: 2 * time.Minute, Decay: 5 * time.Minute}) {
		t.Errorf("DefaultFast = %+v, want the issue's 10s, 1.5 °C/min, 2m, 5m", DefaultFast)
	}

	c, err = Load([]byte(valid + "sweep:\n  fast: {hold: 0s, trigger_c_per_min: 3}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultFast
	want.Hold, want.TriggerCPerMin = 0, 3
	if *c.Sweep.Fast != want {
		t.Errorf("explicit hold: 0s and trigger 3: got %+v, want %+v", *c.Sweep.Fast, want)
	}
}

func TestFastRejects(t *testing.T) {
	// valid has 2 sensors, so the G6 minimum is 1.6 s and the 2 s floor
	// is the stricter rule; tenSensors makes G6 the stricter one (8 s).
	tenSensors := valid + `  - device_id: second
    sensors:
      - {rom: 28-000000000001, position: p1, order: 1}
      - {rom: 28-000000000002, position: p2, order: 2}
      - {rom: 28-000000000003, position: p3, order: 3}
      - {rom: 28-000000000004, position: p4, order: 4}
      - {rom: 28-000000000005, position: p5, order: 5}
      - {rom: 28-000000000006, position: p6, order: 6}
      - {rom: 28-000000000007, position: p7, order: 7}
      - {rom: 28-000000000008, position: p8, order: 8}
`
	tests := []struct {
		name string
		yaml string
		want string // substring of the error
	}{
		{"fast as slow as slow", strings.Replace(fastValid, "interval: 10s", "interval: 30s", 1), "sweep.fast.interval: 30s must be shorter than sweep.interval (30s)"},
		{"fast slower than slow", strings.Replace(fastValid, "interval: 10s", "interval: 1m", 1), "must be shorter than sweep.interval"},
		{"fast shorter than one clean read (G6)", tenSensors + "sweep:\n  fast: {interval: 7s}\n", "sweep.fast.interval: 7s is shorter than one clean read of all 10 sensors (8s)"},
		{"fast under the 2 s floor", valid + "sweep:\n  fast: {interval: 1900ms}\n", "sweep.fast.interval: 1.9s is under 2s"},
		{"zero fast interval", valid + "sweep:\n  fast: {interval: 0s}\n", "sweep.fast.interval"},
		{"zero trigger", valid + "sweep:\n  fast: {trigger_c_per_min: 0}\n", "sweep.fast.trigger_c_per_min: 0 must be greater than 0"},
		{"negative trigger", valid + "sweep:\n  fast: {trigger_c_per_min: -1.5}\n", "trigger_c_per_min: -1.5 must be greater than 0"},
		{"NaN trigger", valid + "sweep:\n  fast: {trigger_c_per_min: .nan}\n", "trigger_c_per_min"},
		{"infinite trigger", valid + "sweep:\n  fast: {trigger_c_per_min: .inf}\n", "trigger_c_per_min"},
		{"negative hold", valid + "sweep:\n  fast: {hold: -1s}\n", "sweep.fast.hold: -1s is negative"},
		{"zero decay", valid + "sweep:\n  fast: {decay: 0s}\n", "sweep.fast.decay: 0s must be positive"},
		{"negative decay", valid + "sweep:\n  fast: {decay: -5m}\n", "sweep.fast.decay"},
		// A typo inside the block must fail loudly, like everywhere else.
		{"unknown fast key", valid + "sweep:\n  fast: {intervl: 10s}\n", "intervl"},
		{"trigger written as a step", valid + "sweep:\n  fast: {trigger_c: 0.5}\n", "trigger_c"},
		{"a scalar instead of a block", valid + "sweep:\n  fast: 10s\n", "yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v\nwant it to mention %q", err, tt.want)
			}
		})
	}
}

func TestFastNearMisses(t *testing.T) {
	// Each just inside a rule's edge, so a rule that is too strict fails here.
	for name, y := range map[string]string{
		"fast at the 2 s floor":       valid + "sweep:\n  fast: {interval: 2s}\n",
		"fast just under slow":        valid + "sweep:\n  fast: {interval: 29999ms}\n",
		"zero hold":                   valid + "sweep:\n  fast: {hold: 0s}\n",
		"a tiny trigger":              valid + "sweep:\n  fast: {trigger_c_per_min: 0.01}\n",
		"a 1 ms decay":                valid + "sweep:\n  fast: {decay: 1ms}\n",
		"a longer slow interval":      valid + "sweep:\n  interval: 5m\n  fast: {interval: 4m}\n",
		"the issue's block, verbatim": fastValid,
	} {
		if _, err := Load([]byte(y)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Every problem in the block is reported at once, like the rest of the file.
func TestFastReportsEveryProblem(t *testing.T) {
	_, err := Load([]byte(valid + "sweep:\n  fast: {interval: 1m, trigger_c_per_min: 0, hold: -1s, decay: 0s}\n"))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"sweep.fast.interval", "sweep.fast.trigger_c_per_min", "sweep.fast.hold", "sweep.fast.decay"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v\nwant it to mention %s", err, want)
		}
	}
}

// fastKeys is UnmarshalYAML's list of known keys; it must name exactly
// Fast's yaml tags, or a real key would be refused (or a typo let through).
func TestFastKeysMatchTheStruct(t *testing.T) {
	typ := reflect.TypeOf(Fast{})
	var tags []string
	for i := 0; i < typ.NumField(); i++ {
		tags = append(tags, typ.Field(i).Tag.Get("yaml"))
	}
	if !reflect.DeepEqual(tags, fastKeys) {
		t.Errorf("Fast's yaml tags %v, fastKeys %v", tags, fastKeys)
	}
}
