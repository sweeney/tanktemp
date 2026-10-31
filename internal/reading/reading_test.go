package reading

import (
	"errors"
	"testing"
)

func milli(v int) *int { return &v }

func TestCodesAreTheWireVocabulary(t *testing.T) {
	// These strings are part of the MQTT contract (docs/WIRE.md). Renaming
	// one is a breaking change, so the full list is pinned here.
	want := []Code{NotPresent, BusLow, CRC, PowerOnReset, BadScratchpad, OutOfRange, Malformed, ReadError}
	wantStr := []string{"not_present", "bus_low", "crc", "power_on_reset", "bad_scratchpad", "out_of_range", "malformed", "read_error"}
	got := FailureCodes()
	if len(got) != len(want) {
		t.Fatalf("FailureCodes() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] || string(got[i]) != wantStr[i] {
			t.Errorf("code %d = %q, want %q", i, got[i], wantStr[i])
		}
	}
	if OK.Failed() || Cancelled.Failed() {
		t.Error("OK and Cancelled are not failures")
	}
	for _, c := range want {
		if !c.Failed() {
			t.Errorf("%s should be a failure", c)
		}
	}
}

func TestReadingCheck(t *testing.T) {
	tests := []struct {
		name string
		r    Reading
		want error
	}{
		{"ok with value", Reading{ROM: "28-000000000001", Code: OK, MilliC: milli(17000), Attempts: 1}, nil},
		{"ok without value", Reading{ROM: "28-000000000001", Code: OK, Attempts: 1}, ErrInconsistent},
		{"failure with value", Reading{ROM: "28-000000000001", Code: CRC, MilliC: milli(1), Attempts: 3}, ErrInconsistent},
		{"failure without value", Reading{ROM: "28-000000000001", Code: CRC, Attempts: 3}, nil},
		{"no rom", Reading{Code: NotPresent, Attempts: 1}, ErrInconsistent},
		{"no attempts", Reading{ROM: "28-000000000001", Code: NotPresent}, ErrInconsistent},
		{"cancelled may have no attempts", Reading{ROM: "28-000000000001", Code: Cancelled}, nil},
		{"unknown code", Reading{ROM: "28-000000000001", Code: "melted", Attempts: 1}, ErrInconsistent},
		{"suspect on a failure", Reading{ROM: "28-000000000001", Code: CRC, Attempts: 1, Suspect: Suspect85}, ErrInconsistent},
		{"suspect on ok", Reading{ROM: "28-000000000001", Code: OK, MilliC: milli(85000), Attempts: 2, Suspect: Suspect85}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.r.Check(); !errors.Is(err, tt.want) {
				t.Errorf("Check() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTopology(t *testing.T) {
	top := NewTopology(map[string][]string{
		"w1_bus_master2": {"28-000000000003", "28-000000000001"},
		"w1_bus_master1": {"28-000000000002"},
	})
	if bus, ok := top.Bus("28-000000000001"); !ok || bus != "w1_bus_master2" {
		t.Errorf("Bus = %q, %v", bus, ok)
	}
	if _, ok := top.Bus("28-0000000000ff"); ok {
		t.Error("absent ROM reported present")
	}
	got := top.ROMs()
	want := []string{"28-000000000001", "28-000000000002", "28-000000000003"}
	if len(got) != len(want) {
		t.Fatalf("ROMs = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ROMs[%d] = %s, want %s (sorted)", i, got[i], want[i])
		}
	}
	buses := top.Buses()
	if len(buses) != 2 || buses[0] != "w1_bus_master1" {
		t.Errorf("Buses = %v, want sorted", buses)
	}
	if ids := top.OnBus("w1_bus_master2"); len(ids) != 2 || ids[0] != "28-000000000001" {
		t.Errorf("OnBus = %v, want sorted", ids)
	}
	var zero Topology
	if len(zero.ROMs()) != 0 {
		t.Error("zero Topology should be empty")
	}
}

func TestCelsius(t *testing.T) {
	if c, ok := (Reading{Code: OK, MilliC: milli(-10125)}).Celsius(); !ok || c != -10.125 {
		t.Errorf("Celsius = %v, %v", c, ok)
	}
	if _, ok := (Reading{Code: CRC}).Celsius(); ok {
		t.Error("a failed reading has no Celsius")
	}
}
