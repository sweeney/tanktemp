package onewire

import (
	"os"
	"slices"
	"testing"
)

// BusPins reads which GPIO each loaded w1-gpio overlay is on, from its
// device-tree node's gpios property (#10): the pin is the second cell. A
// node's name is its unit address (the pin in hex), but the property is
// what the driver actually uses.
func TestBusPins(t *testing.T) {
	for _, tt := range []struct {
		dir  string
		want []int
		err  bool
	}{
		{"rig-2026-11-21", []int{4, 17}, false},
		{"none", nil, false},
		// A node is a bus only if enabled and bound to its driver (review on #11).
		{"disabled", []int{4}, false},
		{"unbound", []int{4}, false},
		{"malformed", nil, true},
	} {
		got, err := BusPins(os.DirFS("testdata/devicetree/" + tt.dir))
		if (err != nil) != tt.err || !slices.Equal(got, tt.want) {
			t.Errorf("%s: BusPins = %v, %v; want %v (error %v)", tt.dir, got, err, tt.want, tt.err)
		}
	}
}

// No device tree at all (not a Pi) is an error, not "no buses".
func TestBusPinsWithoutADeviceTree(t *testing.T) {
	if _, err := BusPins(os.DirFS(t.TempDir())); err == nil {
		t.Error("no device tree read as no buses")
	}
}
