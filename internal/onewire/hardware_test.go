//go:build hardware

package onewire

// The hardware check: run on the Pi, read-only, never in CI. It reads the
// real sensors through the real Source and checks every configured ROM is
// present, reads OK first time, is in powered mode and at 12-bit. See the
// README ("Hardware check") for building and running it:
//
//	TANKTEMP_ROMS=28-…,28-… ./onewire.test -test.run Hardware -test.v

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sweeney/tanktemp/internal/reading"
)

func TestHardware(t *testing.T) {
	roms := strings.FieldsFunc(os.Getenv("TANKTEMP_ROMS"), func(r rune) bool { return r == ',' || r == ' ' })
	if len(roms) == 0 {
		t.Fatal("set TANKTEMP_ROMS to the configured ROM IDs, comma separated")
	}
	root := os.Getenv("W1_ROOT")
	if root == "" {
		root = DevicesDir
	}
	src := NewSource(root, 1) // one attempt: a healthy rig needs no retries
	ctx := context.Background()
	top, err := src.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := src.Read(ctx, top, roms)
	for _, rom := range roms {
		r := got[rom]
		bus, present := top.Bus(rom)
		switch {
		case !present:
			t.Errorf("%s: not on any bus", rom)
		case r.Code != reading.OK:
			t.Errorf("%s (%s): %s", rom, bus, r.Code)
		case r.Suspect != "":
			t.Errorf("%s (%s): suspect %s", rom, bus, r.Suspect)
		default:
			c, _ := r.Celsius()
			t.Logf("%s on %s: %.4f °C in %v", rom, bus, c, r.Took.Round(1e6))
		}
		info, err := src.Inspect(ctx, rom)
		if err != nil {
			t.Errorf("%s: Inspect: %v", rom, err)
			continue
		}
		if info.ExtPower == nil || !*info.ExtPower {
			t.Errorf("%s: not in powered mode (ext_power %v)", rom, info.ExtPower)
		}
		if info.Resolution == nil || *info.Resolution != 12 {
			t.Errorf("%s: resolution %v, want 12", rom, info.Resolution)
		}
	}
	for _, rom := range top.ROMs() {
		found := false
		for _, r := range roms {
			found = found || r == rom
		}
		if !found {
			t.Logf("note: %s is on a bus but not configured", rom)
		}
	}
}
