package onewire

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedFromFixtures adds every captured or synthetic file named name.
func seedFromFixtures(f *testing.F, name string) {
	f.Helper()
	err := filepath.WalkDir(scenarioRoot, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			data, rerr := os.ReadFile(p)
			if rerr == nil {
				f.Add(data)
			}
		}
		return err
	})
	if err != nil {
		f.Fatal(err)
	}
}

var sentinels = []error{ErrMalformed, ErrCRC, ErrBusLow, ErrBadScratchpad, ErrPowerOnReset, ErrOutOfRange}

func known(err error) bool {
	for _, s := range sentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// Whatever the kernel (or a failing bus) hands us: never panic, every error
// is one we can classify, and anything accepted is self-consistent and agrees
// with the kernel's own t= value.
func FuzzParseW1Slave(f *testing.F) {
	seedFromFixtures(f, "w1_slave")
	f.Add([]byte(""))
	f.Add([]byte("\x00\x00\x00"))
	f.Add([]byte("27 01 4b 46 7f ff 09 10 72 : crc=72 YES\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		w, err := ParseW1Slave(data)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("parse error %v is not ErrMalformed", err)
			}
			return
		}
		cerr := w.Check()
		if cerr != nil {
			if !known(cerr) {
				t.Fatalf("Check error %v is not a known sentinel", cerr)
			}
			return
		}
		if !w.KernelCRC || !w.Scratchpad.CRCValid() {
			t.Fatal("accepted a reading without a valid CRC")
		}
		mc := w.Scratchpad.MilliC()
		if mc < -55000 || mc > 125000 {
			t.Fatalf("accepted %d m°C, out of range", mc)
		}
		if w.Scratchpad == (Scratchpad{}) {
			t.Fatal("accepted an all-zero scratchpad")
		}
	})
}

// Check over arbitrary scratchpads: never panics, only known errors, and an
// accepted one is in range with a valid CRC.
func FuzzScratchpadCheck(f *testing.F) {
	f.Add([]byte{0x27, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x09, 0x10, 0x72})
	f.Add(make([]byte, 9))
	f.Add([]byte{0x50, 0x05, 0x4b, 0x46, 0x7f, 0xff, 0x0c, 0x10, 0x1c})
	f.Fuzz(func(t *testing.T, b []byte) {
		var s Scratchpad
		copy(s[:], b)
		err := s.Check()
		if err != nil {
			if !known(err) {
				t.Fatalf("unknown error %v", err)
			}
			return
		}
		if !s.CRCValid() {
			t.Fatal("accepted a bad CRC")
		}
		if mc := s.MilliC(); mc < -55000 || mc > 125000 {
			t.Fatalf("accepted %d", mc)
		}
	})
}

// The bus master's device list: never panics, returns only non-empty,
// trimmed, sorted IDs, and never the kernel's "not found." placeholder.
func FuzzMasterSlaves(f *testing.F) {
	seedFromFixtures(f, "w1_master_slaves")
	f.Add([]byte("not found.\n"))
	f.Add([]byte("\n\n  \n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		ids := parseMasterSlaves(data)
		for i, id := range ids {
			if id == "" || id != strings.TrimSpace(id) || id == "not found." || strings.ContainsAny(id, "\n\r") {
				t.Fatalf("bad id %q", id)
			}
			if i > 0 && ids[i-1] > id {
				t.Fatalf("not sorted: %v", ids)
			}
		}
	})
}

// Every one of the 65,536 temperature register values decodes as the
// kernel's integer formula does, checked here against independent float
// arithmetic, and is accepted exactly when it is in the DS18B20's range.
func TestEveryRawValue(t *testing.T) {
	for raw := 0; raw <= 0xFFFF; raw++ {
		lsb, msb := byte(raw), byte(raw>>8)
		b6 := 0x10 - lsb&0x0F
		s := Scratchpad{lsb, msb, 0x4b, 0x46, 0x7f, 0xff, b6, 0x10}
		s[8] = CRC8(s[:8])
		want := int(math.Trunc(float64(int16(uint16(raw))) * 62.5))
		if got := s.MilliC(); got != want {
			t.Fatalf("raw %#04x: MilliC %d, want %d", raw, got, want)
		}
		err := s.Check()
		inRange := want >= -55000 && want <= 125000
		switch {
		case inRange && err != nil:
			t.Fatalf("raw %#04x (%d m°C): rejected: %v", raw, want, err)
		case !inRange && !errors.Is(err, ErrOutOfRange):
			t.Fatalf("raw %#04x (%d m°C): err %v, want ErrOutOfRange", raw, want, err)
		}
	}
}
