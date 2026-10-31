package onewire

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
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
		buses = append(buses, Bus{Name: m, Devices: parseMasterSlaves(data)})
	}
	return buses, nil
}

// parseMasterSlaves reads a w1_master_slaves file: one device ID per line,
// or the kernel's "not found." when the bus is empty. IDs come back sorted.
func parseMasterSlaves(data []byte) []string {
	var ids []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "not found." {
			continue
		}
		// IDs never contain whitespace, so any (a stray \r, say) separates.
		ids = append(ids, strings.Fields(line)...)
	}
	sort.Strings(ids)
	return ids
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
