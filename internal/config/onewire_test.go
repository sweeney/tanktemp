package config

import (
	"strings"
	"testing"
)

const withBuses = valid + `onewire:
  buses:
    - {gpio: 4, label: D1}
    - {gpio: 17}
`

// The 1-Wire bus pins are declared in the config (#10), so the config, not
// setup-pi's default, says which pin is which.
func TestOneWireBuses(t *testing.T) {
	c, err := Load([]byte(withBuses))
	if err != nil {
		t.Fatal(err)
	}
	b := c.OneWire.Buses
	if len(b) != 2 || b[0].GPIO == nil || *b[0].GPIO != 4 || b[0].Label != "D1" || *b[1].GPIO != 17 || b[1].Label != "" {
		t.Errorf("buses = %+v", b)
	}
	if got := c.OneWire.Pins(); len(got) != 2 || got[0] != 4 || got[1] != 17 {
		t.Errorf("Pins() = %v", got)
	}
}

// Optional: a config from before #10 still loads (a new build must accept
// the config already on the Pi), and declares no pins.
func TestOneWireIsOptional(t *testing.T) {
	c, err := Load([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.OneWire.Buses != nil || c.OneWire.Pins() != nil {
		t.Errorf("onewire = %+v", c.OneWire)
	}
}

func TestOneWireRejects(t *testing.T) {
	for _, tt := range []struct{ name, yaml, want string }{
		{"empty block", valid + "onewire:\n", "onewire"},
		{"no buses", valid + "onewire: {buses: []}\n", "onewire.buses"},
		{"a bare bus", valid + "onewire:\n  buses:\n    -\n", "onewire.buses[0]"},
		{"unknown key", valid + "onewire: {buses: [{gpio: 4, pin: 4}]}\n", "pin"},
		{"unknown block key", valid + "onewire: {busses: [{gpio: 4}]}\n", "busses"},
		{"no gpio", valid + "onewire: {buses: [{label: D1}]}\n", "onewire.buses[0].gpio"},
		{"gpio off the header", valid + "onewire: {buses: [{gpio: 28}]}\n", "onewire.buses[0].gpio"},
		{"gpio 0, the HAT ID pin", valid + "onewire: {buses: [{gpio: 0}]}\n", "HAT"},
		// yaml.v3 decodes 4.5 into an int as 4 (review on #11): refused, not truncated.
		{"a fractional gpio", valid + "onewire: {buses: [{gpio: 4.5}]}\n", "onewire.buses[0].gpio"},
		{"an exponent gpio", valid + "onewire: {buses: [{gpio: 1e1}]}\n", "onewire.buses[0].gpio"},
		{"a fractional meter gpio", valid + "meters:\n  - {device_id: hot_water_meter, gpio: 27.9, litres_per_pulse: 1}\n", "meters[0].gpio"},
		{"a pin twice", valid + "onewire: {buses: [{gpio: 4}, {gpio: 4}]}\n", "onewire.buses[0]"},
		// Today this loads, and fails only at runtime as gpio_request_failed.
		{"the meter on a bus pin", withBuses + "meters:\n  - {device_id: hot_water_meter, gpio: 4, litres_per_pulse: 1}\n", "onewire.buses[0]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A pin used twice is named with both uses, and their lines.
func TestOneWirePinUsedTwiceNamesBoth(t *testing.T) {
	_, err := Load([]byte(withBuses + "meters:\n  - {device_id: hot_water_meter, gpio: 17, litres_per_pulse: 1}\n"))
	if err == nil || !strings.Contains(err.Error(), "GPIO 17") || !strings.Contains(err.Error(), "onewire.buses[1]") || !strings.Contains(err.Error(), "meters[0]") {
		t.Errorf("err = %v", err)
	}
}
