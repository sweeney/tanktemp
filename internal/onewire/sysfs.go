package onewire

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// DevicesDir is where the w1 subsystem exposes buses and devices.
const DevicesDir = "/sys/bus/w1/devices"

// FamilyDS18B20 is the first byte of every DS18B20 ROM ID ("28-…" in sysfs).
const FamilyDS18B20 = "28"

// SysFS reads sensors from a filesystem laid out like /sys/bus/w1/devices.
// Use NewSysFS for the real thing; tests pass an fstest.MapFS.
type SysFS struct {
	FS fs.FS
}

// NewSysFS returns a reader rooted at dir (normally DevicesDir).
func NewSysFS(dir string) SysFS { return SysFS{FS: os.DirFS(dir)} }

// Bus is one w1 bus master and the devices it currently sees.
//
// Bus names (w1_bus_master1, …) are assigned in probe order and are NOT stable
// across reboots: on 2026-10-18 the GPIO4 and GPIO17 buses swapped numbers.
// Identify sensors by ID; treat Bus as diagnostic information only.
type Bus struct {
	Name    string
	Devices []string // all IDs, including non-DS18B20 and junk "00-…" entries
}

// Buses lists every bus master and its devices, sorted by name.
func (s SysFS) Buses() ([]Bus, error) {
	masters, err := fs.Glob(s.FS, "w1_bus_master*")
	if err != nil {
		return nil, err
	}
	sort.Strings(masters)
	var buses []Bus
	for _, m := range masters {
		data, err := fs.ReadFile(s.FS, m+"/w1_master_slaves")
		if err != nil {
			return nil, fmt.Errorf("read %s slaves: %w", m, err)
		}
		b := Bus{Name: m}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			// The kernel prints "not found." when a bus has no devices.
			if line = strings.TrimSpace(line); line != "" && line != "not found." {
				b.Devices = append(b.Devices, line)
			}
		}
		sort.Strings(b.Devices)
		buses = append(buses, b)
	}
	return buses, nil
}

// Thermometers returns DS18B20 IDs grouped by bus, ignoring anything else
// (notably the fake "00-…" devices the bus search invents from noise while
// wiring is being changed).
func (s SysFS) Thermometers() (map[string][]string, error) {
	buses, err := s.Buses()
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, b := range buses {
		for _, id := range b.Devices {
			if strings.HasPrefix(id, FamilyDS18B20+"-") {
				out[b.Name] = append(out[b.Name], id)
			}
		}
	}
	return out, nil
}

// ErrNotPresent: the device isn't in sysfs (unplugged, or dropped off after
// failed bus searches). Distinct from read errors on a present device.
var ErrNotPresent = errors.New("device not present")

// Read triggers a conversion and returns the parsed result. It blocks for the
// conversion time (~750 ms at 12-bit) — the kernel does a fresh conversion on
// every read of w1_slave.
func (s SysFS) Read(id string) (W1Slave, error) {
	data, err := fs.ReadFile(s.FS, id+"/w1_slave")
	if errors.Is(err, fs.ErrNotExist) {
		return W1Slave{}, fmt.Errorf("%s: %w", id, ErrNotPresent)
	}
	if err != nil {
		return W1Slave{}, fmt.Errorf("%s: read w1_slave: %w", id, err)
	}
	w, err := ParseW1Slave(data)
	if err != nil {
		return w, fmt.Errorf("%s: %w", id, err)
	}
	return w, nil
}

// Reading is the outcome of reading one sensor, after retries.
type Reading struct {
	ID       string
	MilliC   int           // valid only when Err == nil
	Attempts int           // reads performed (1 = first read was good)
	Took     time.Duration // wall time including retries
	Err      error         // last error if every attempt failed
}

// Celsius is MilliC as a float.
func (r Reading) Celsius() float64 { return float64(r.MilliC) / 1000 }

// ReadChecked reads id up to attempts times, stopping at the first reading that
// passes every check. Retrying is cheap and effective: each read is a fresh
// conversion, so a CRC glitch or a power-on-reset 85 °C is gone on the next try.
// Persistent faults (unplugged, bus held low) fail all attempts.
func (s SysFS) ReadChecked(id string, attempts int) Reading {
	if attempts < 1 {
		attempts = 1
	}
	start := time.Now()
	r := Reading{ID: id}
	for r.Attempts < attempts {
		r.Attempts++
		w, err := s.Read(id)
		if err == nil {
			err = w.Check()
		}
		if err == nil {
			r.MilliC, r.Err = w.Scratchpad.MilliC(), nil
			break
		}
		r.Err = err
		if errors.Is(err, ErrNotPresent) {
			break // retrying won't make it appear
		}
	}
	r.Took = time.Since(start)
	return r
}

// ReadAll reads every id. With parallel, each bus is read in its own goroutine
// (devices on one bus are always serialised by the kernel). Measured on a Pi
// Zero 2 W with 2×5 sensors: 8.9 s sequential, 7.2 s parallel.
func (s SysFS) ReadAll(byBus map[string][]string, attempts int, parallel bool) map[string]Reading {
	out := map[string]Reading{}
	var mu sync.Mutex
	readBus := func(ids []string) {
		for _, id := range ids {
			r := s.ReadChecked(id, attempts)
			mu.Lock()
			out[id] = r
			mu.Unlock()
		}
	}
	buses := make([]string, 0, len(byBus))
	for b := range byBus {
		buses = append(buses, b)
	}
	sort.Strings(buses)
	if !parallel {
		for _, b := range buses {
			readBus(byBus[b])
		}
		return out
	}
	var wg sync.WaitGroup
	for _, b := range buses {
		wg.Add(1)
		go func(ids []string) { defer wg.Done(); readBus(ids) }(byBus[b])
	}
	wg.Wait()
	return out
}
