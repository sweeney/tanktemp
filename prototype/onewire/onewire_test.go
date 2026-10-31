package onewire

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// slave renders w1_slave text for the 8 given bytes, with a correct CRC and
// the kernel's verdict and t= value, the way the kernel would.
func slave(b ...byte) string {
	var s Scratchpad
	copy(s[:], b)
	s[8] = CRC8(s[:8])
	return render(s)
}

func render(s Scratchpad) string {
	hex := make([]string, 9)
	for i, b := range s {
		hex[i] = fmt.Sprintf("%02x", b)
	}
	verdict := "NO"
	if s.CRCValid() {
		verdict = "YES"
	}
	h := strings.Join(hex, " ")
	return fmt.Sprintf("%s : crc=%02x %s\n%s t=%d\n", h, s[8], verdict, h, s.MilliC())
}

func TestCRC8KnownValues(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want byte
	}{
		// ROM ID of our first sensor, read from /sys/bus/w1/devices/28-000011721ab6/id.
		{"rom id 28-000011721ab6", []byte{0x28, 0xb6, 0x1a, 0x72, 0x11, 0x00, 0x00}, 0x24},
		// First scratchpad ever read, 2026-09-25 (18.437 °C).
		{"scratchpad 18.437", []byte{0x27, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x09, 0x10}, 0x72},
		// All zeros has CRC zero — which is why a held-low bus passes the kernel's check.
		{"all zeros", make([]byte, 8), 0x00},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CRC8(tt.data); got != tt.want {
				t.Errorf("CRC8 = %#02x, want %#02x", got, tt.want)
			}
		})
	}
}

// Every capture from the real 10-sensor rig must parse, agree with the kernel,
// and pass all checks.
func TestRealCaptures(t *testing.T) {
	data, err := os.ReadFile("testdata/w1_slave-raw.txt")
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(string(data), "== ")[1:]
	if len(blocks) != 10 {
		t.Fatalf("want 10 captures, got %d", len(blocks))
	}
	for _, b := range blocks {
		id, body, _ := strings.Cut(b, "\n")
		t.Run(id, func(t *testing.T) {
			w, err := ParseW1Slave([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Check(); err != nil {
				t.Errorf("Check: %v", err)
			}
			if got := w.Scratchpad.MilliC(); got != w.KernelT {
				t.Errorf("MilliC = %d, kernel t = %d", got, w.KernelT)
			}
			if w.Scratchpad.Resolution() != 12 {
				t.Errorf("resolution %d", w.Scratchpad.Resolution())
			}
			// Genuine-part pattern relied on by the power-on-reset check.
			if want := 0x10 - w.Scratchpad[0]&0x0F; w.Scratchpad[6] != want {
				t.Errorf("byte 6 = %#02x, want %#02x", w.Scratchpad[6], want)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		wantMC int
		want   error
	}{
		{"normal", slave(0x27, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x09, 0x10), 18437, nil},
		{"negative -10.125", slave(0x5e, 0xff, 0x4b, 0x46, 0x7f, 0xff, 0x02, 0x10), -10125, nil},
		{"tank hot 64.5", slave(0x08, 0x04, 0x4b, 0x46, 0x7f, 0xff, 0x08, 0x10), 64500, nil},
		{"genuine 85.000 after conversion", slave(0x50, 0x05, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10), 85000, nil},
		{"real 0.000 (ice bath)", slave(0x00, 0x00, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10), 0, nil},
		{"power-on reset 85", slave(0x50, 0x05, 0x4b, 0x46, 0x7f, 0xff, 0x0c, 0x10), 0, ErrPowerOnReset},
		{"bus held low", "00 00 00 00 00 00 00 00 00 : crc=00 YES\n00 00 00 00 00 00 00 00 00 t=0\n", 0, ErrBusLow},
		{"bus floating high", "ff ff ff ff ff ff ff ff ff : crc=c9 NO\nff ff ff ff ff ff ff ff ff t=-62\n", 0, ErrCRC},
		{"kernel CRC NO", "27 01 4b 46 7f ff 09 10 73 : crc=73 NO\n27 01 4b 46 7f ff 09 10 73 t=18437\n", 0, ErrCRC},
		{"kernel YES but bytes disagree", "27 01 4b 46 7f ff 09 10 73 : crc=73 YES\n27 01 4b 46 7f ff 09 10 73 t=18437\n", 0, ErrCRC},
		{"bad config byte", slave(0x27, 0x01, 0x4b, 0x46, 0x00, 0xff, 0x09, 0x10), 0, ErrBadScratchpad},
		{"bad byte 7", slave(0x27, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x09, 0x00), 0, ErrBadScratchpad},
		{"above 125", slave(0xe0, 0x07, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10), 0, ErrOutOfRange}, // 126 °C
		{"garbage", "hello\n", 0, ErrMalformed},
		{"no t=", "27 01 4b 46 7f ff 09 10 72 : crc=72 YES\n27 01 4b 46 7f ff 09 10 72\n", 0, ErrMalformed},
		{"short", "27 01 4b : crc=72 YES\n27 01 4b t=1\n", 0, ErrMalformed},
		{"bad hex", "zz 01 4b 46 7f ff 09 10 72 : crc=72 YES\nzz t=1\n", 0, ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := ParseW1Slave([]byte(tt.text))
			if err == nil {
				err = w.Check()
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if err == nil && w.Scratchpad.MilliC() != tt.wantMC {
				t.Errorf("MilliC = %d, want %d", w.Scratchpad.MilliC(), tt.wantMC)
			}
		})
	}
}

func TestResolution(t *testing.T) {
	for cfg, want := range map[byte]int{0x1f: 9, 0x3f: 10, 0x5f: 11, 0x7f: 12} {
		var s Scratchpad
		s[4] = cfg
		if got := s.Resolution(); got != want {
			t.Errorf("config %#02x: resolution %d, want %d", cfg, got, want)
		}
	}
}

// fixture mirrors /sys/bus/w1/devices after the 2026-10-18 reboot, including a
// junk "00-" device of the kind seen during rewiring.
func fixture() fstest.MapFS {
	good := slave(0x10, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10) // 17.000
	return fstest.MapFS{
		"w1_bus_master1/w1_master_slaves": {Data: []byte("28-00001171f570\n28-000011721aa5\n00-e00000000000\n")},
		"w1_bus_master2/w1_master_slaves": {Data: []byte("28-000011721ce9\n")},
		"w1_bus_master3/w1_master_slaves": {Data: []byte("not found.\n")},
		"28-00001171f570/w1_slave":        {Data: []byte(good)},
		"28-000011721aa5/w1_slave":        {Data: []byte("00 00 00 00 00 00 00 00 00 : crc=00 YES\n00 00 00 00 00 00 00 00 00 t=0\n")},
		"28-000011721ce9/w1_slave":        {Data: []byte(good)},
	}
}

func TestBusesAndThermometers(t *testing.T) {
	s := SysFS{FS: fixture()}
	buses, err := s.Buses()
	if err != nil {
		t.Fatal(err)
	}
	if len(buses) != 3 || len(buses[0].Devices) != 3 || len(buses[2].Devices) != 0 {
		t.Fatalf("buses = %+v", buses)
	}
	th, err := s.Thermometers()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(th["w1_bus_master1"], ","); got != "28-00001171f570,28-000011721aa5" {
		t.Errorf("bus 1 thermometers = %s (junk 00- device must be ignored)", got)
	}
	if len(th["w1_bus_master2"]) != 1 {
		t.Errorf("bus 2 = %v", th["w1_bus_master2"])
	}
}

func TestReadChecked(t *testing.T) {
	s := SysFS{FS: fixture()}

	r := s.ReadChecked("28-00001171f570", 3)
	if r.Err != nil || r.MilliC != 17000 || r.Attempts != 1 {
		t.Errorf("good sensor: %+v", r)
	}

	r = s.ReadChecked("28-000011721aa5", 3)
	if !errors.Is(r.Err, ErrBusLow) || r.Attempts != 3 {
		t.Errorf("bus-low sensor should fail all 3 attempts: %+v", r)
	}

	r = s.ReadChecked("28-0000deadbeef", 3)
	if !errors.Is(r.Err, ErrNotPresent) || r.Attempts != 1 {
		t.Errorf("missing sensor should fail fast: %+v", r)
	}
}

// seqFS serves a different w1_slave body on each read, like a real sensor
// recovering from a power-on reset.
type seqFS struct {
	bodies []string
	n      int
}

func (f *seqFS) Open(name string) (fs.File, error) {
	body := f.bodies[min(f.n, len(f.bodies)-1)]
	f.n++
	return fstest.MapFS{name: {Data: []byte(body)}}.Open(name)
}

func TestReadCheckedRecoversAfterReset(t *testing.T) {
	f := &seqFS{bodies: []string{
		slave(0x50, 0x05, 0x4b, 0x46, 0x7f, 0xff, 0x0c, 0x10), // power-on reset 85
		slave(0x2d, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x03, 0x10), // 18.812
	}}
	r := SysFS{FS: f}.ReadChecked("28-00001171f570", 3)
	if r.Err != nil || r.MilliC != 18812 || r.Attempts != 2 {
		t.Errorf("want recovery on 2nd attempt, got %+v", r)
	}
}

func TestReadAll(t *testing.T) {
	s := SysFS{FS: fixture()}
	th, _ := s.Thermometers()
	for _, parallel := range []bool{false, true} {
		got := s.ReadAll(th, 2, parallel)
		if len(got) != 3 {
			t.Fatalf("parallel=%v: %d readings", parallel, len(got))
		}
		if got["28-000011721ce9"].MilliC != 17000 || got["28-000011721aa5"].Err == nil {
			t.Errorf("parallel=%v: %+v", parallel, got)
		}
	}
}

func TestReadingCelsius(t *testing.T) {
	r := Reading{MilliC: -10125, Took: time.Second}
	if r.Celsius() != -10.125 {
		t.Errorf("Celsius = %v", r.Celsius())
	}
}
