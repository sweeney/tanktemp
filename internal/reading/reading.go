// Package reading is the boundary between tanktemp's business logic and the
// 1-Wire hardware. It defines what a sensor reading is and the Source
// interface that produces them, and nothing about files, buses as
// directories, or DS18B20 scratchpads: that is all internal/onewire's
// business. The core packages see sensors only through this package, so they
// can be tested with readingtest.Fake and no hardware at all.
package reading

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Code classifies the outcome of reading one sensor. The failure codes'
// strings are part of the MQTT wire contract (docs/WIRE.md): never rename one.
type Code string

const (
	// OK: a real measurement; Reading.MilliC is set.
	OK Code = "ok"

	// NotPresent: the sensor isn't on any bus (unplugged, or dropped after
	// failed bus searches).
	NotPresent Code = "not_present"
	// BusLow: every scratchpad byte read as 0x00, so the data line was held
	// low. Passes the kernel's CRC, which is why it needs its own code.
	BusLow Code = "bus_low"
	// CRC: the scratchpad failed its CRC. Usually noise or a marginal contact.
	CRC Code = "crc"
	// PowerOnReset: the sensor restarted and reported its 85 °C power-up value.
	PowerOnReset Code = "power_on_reset"
	// BadScratchpad: CRC passed but fixed bytes are wrong; not a sane DS18B20.
	BadScratchpad Code = "bad_scratchpad"
	// OutOfRange: outside the sensor's measurable range.
	OutOfRange Code = "out_of_range"
	// Malformed: the kernel's output couldn't be parsed.
	Malformed Code = "malformed"
	// ReadError: any other failure. Unrecognised errors land here rather
	// than being guessed at.
	ReadError Code = "read_error"

	// Cancelled: the sweep was cancelled (shutdown) before this sensor was
	// read. Never published: a cancelled sweep is discarded.
	Cancelled Code = "cancelled"
)

var failureCodes = []Code{NotPresent, BusLow, CRC, PowerOnReset, BadScratchpad, OutOfRange, Malformed, ReadError}

// FailureCodes lists every failure code, in a fixed order.
func FailureCodes() []Code { return append([]Code(nil), failureCodes...) }

// Failed reports whether c is a failure (not OK, not Cancelled).
func (c Code) Failed() bool {
	for _, f := range failureCodes {
		if c == f {
			return true
		}
	}
	return false
}

func (c Code) known() bool { return c == OK || c == Cancelled || c.Failed() }

// Suspect85 flags a reading of exactly 85.000 °C that passed every check and
// survived a retry. 85 °C is also the DS18B20's power-on value, and until a
// real power-on-reset scratchpad has been captured on this hardware, a
// persistent 85.000 can't be fully trusted. It is published, not dropped.
const Suspect85 = "85c_unverified"

// Reading is the outcome of reading one sensor, after any retries.
type Reading struct {
	ROM      string
	Bus      string // diagnostic only: bus names aren't stable across reboots
	MilliC   *int   // raw (uncalibrated) millidegrees; set only when Code == OK
	Code     Code
	Suspect  string // "" or Suspect85; only on an OK reading
	Attempts int    // reads performed; 0 only when Cancelled
	Took     time.Duration
}

// ErrInconsistent: a Reading breaks one of the rules every Source must keep.
var ErrInconsistent = errors.New("inconsistent reading")

// Check reports whether r keeps the rules every Source must keep: a value if
// and only if OK, a ROM, at least one attempt unless cancelled, a known code,
// and a suspect flag only on an OK reading.
func (r Reading) Check() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInconsistent, r.ROM, fmt.Sprintf(format, args...))
	}
	switch {
	case r.ROM == "":
		return bad("no ROM")
	case !r.Code.known():
		return bad("unknown code %q", r.Code)
	case r.Code == OK && r.MilliC == nil:
		return bad("OK without a value")
	case r.Code != OK && r.MilliC != nil:
		return bad("%s with a value", r.Code)
	case r.Code != Cancelled && r.Attempts < 1:
		return bad("%s after %d attempts", r.Code, r.Attempts)
	case r.Suspect != "" && r.Code != OK:
		return bad("suspect flag on %s", r.Code)
	}
	return nil
}

// Celsius returns the raw value in °C, and false when there isn't one.
func (r Reading) Celsius() (float64, bool) {
	if r.Code != OK || r.MilliC == nil {
		return 0, false
	}
	return float64(*r.MilliC) / 1000, true
}

// Topology is which thermometer ROMs are on which bus right now. Only
// thermometers the Source can read appear; junk devices are left out.
type Topology struct {
	byROM map[string]string
}

// NewTopology builds a Topology from bus name → ROMs.
func NewTopology(byBus map[string][]string) Topology {
	t := Topology{byROM: map[string]string{}}
	for bus, roms := range byBus {
		for _, rom := range roms {
			t.byROM[rom] = bus
		}
	}
	return t
}

// Bus returns the bus rom is on, and whether it is present at all.
func (t Topology) Bus(rom string) (string, bool) {
	b, ok := t.byROM[rom]
	return b, ok
}

// ROMs lists every present ROM, sorted.
func (t Topology) ROMs() []string {
	out := make([]string, 0, len(t.byROM))
	for rom := range t.byROM {
		out = append(out, rom)
	}
	sort.Strings(out)
	return out
}

// Buses lists the bus names that have at least one thermometer, sorted.
func (t Topology) Buses() []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range t.byROM {
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

// OnBus lists the ROMs on bus, sorted.
func (t Topology) OnBus(bus string) []string {
	var out []string
	for rom, b := range t.byROM {
		if b == bus {
			out = append(out, rom)
		}
	}
	sort.Strings(out)
	return out
}

// Info is what a startup check wants to know about one sensor. A nil field
// means it couldn't be read.
type Info struct {
	ExtPower   *bool // true = powered mode; false = parasite power
	Resolution *int  // bits, 9..12
}

// Source produces readings. internal/onewire is the real one;
// readingtest.Fake is the scriptable one the core packages are tested with.
// readingtest.Contract states the rules every Source must keep.
type Source interface {
	// Discover reports which thermometers are on which bus right now.
	Discover(ctx context.Context) (Topology, error)
	// Read reads each of roms, using t to group the reads by bus. A ROM
	// absent from t is still attempted, so a sensor that has vanished
	// reports NotPresent rather than disappearing. Retries and parallelism
	// are the Source's job. Every requested ROM gets a Reading. When ctx is
	// cancelled it returns promptly, marking unread ROMs Cancelled.
	Read(ctx context.Context, t Topology, roms []string) map[string]Reading
	// Inspect reads a sensor's configuration, for startup checks.
	Inspect(ctx context.Context, rom string) (Info, error)
}
