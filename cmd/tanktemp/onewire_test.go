package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oneTank = "mqtt: {broker: tcp://b:1883}\ntanks:\n  - device_id: t\n    sensors:\n      - {rom: 28-0000117287e6, position: top, order: 1}\n"

func writeCfg(t *testing.T, y string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// -check-config names the 1-Wire buses' pins (#10), before the meter and
// "publishing to" (which deploy/lib.sh reads from the end).
func TestCheckConfigShowsTheBuses(t *testing.T) {
	p := writeCfg(t, oneTank+"onewire:\n  buses:\n    - {gpio: 4, label: D1}\n    - {gpio: 17}\n")
	var out strings.Builder
	if code := realMain([]string{"-config", p, "-check-config"}, &out); code != 0 ||
		!strings.Contains(out.String(), ", verbose log off, 1-Wire buses on GPIO 4 (D1) and 17, publishing to ") {
		t.Errorf("code %d, out %q", code, out.String())
	}
}

// Without them it says so: setup-pi.sh needs them.
func TestCheckConfigWarnsWithoutBuses(t *testing.T) {
	p := writeCfg(t, oneTank)
	var out strings.Builder
	if code := realMain([]string{"-config", p, "-check-config"}, &out); code != 0 ||
		!strings.Contains(out.String(), ", 1-Wire bus pins NOT DECLARED (onewire.buses; setup-pi.sh needs them), publishing to ") {
		t.Errorf("code %d, out %q", code, out.String())
	}
}

// -print-w1-gpios is setup-pi.sh's source for the overlay lines: the pins,
// comma-separated, in config order, and nothing else.
func TestPrintW1GPIOs(t *testing.T) {
	p := writeCfg(t, oneTank+"onewire:\n  buses:\n    - {gpio: 17}\n    - {gpio: 4}\n")
	var out strings.Builder
	if code := realMain([]string{"-config", p, "-print-w1-gpios"}, &out); code != 0 || out.String() != "17,4\n" {
		t.Errorf("code %d, out %q", code, out.String())
	}
}

// With none declared it fails (2) and says what to add, printing no pins.
func TestPrintW1GPIOsWithoutBuses(t *testing.T) {
	p := writeCfg(t, oneTank)
	var out strings.Builder
	if code := realMain([]string{"-config", p, "-print-w1-gpios"}, &out); code != 2 || !strings.Contains(out.String(), "onewire.buses") {
		t.Errorf("code %d, out %q", code, out.String())
	}
}
