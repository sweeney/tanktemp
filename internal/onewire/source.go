package onewire

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
)

// ErrNoBuses: no w1 bus masters exist, so the w1-gpio overlay isn't loaded
// (or w1_root points somewhere wrong).
var ErrNoBuses = errors.New("no 1-Wire bus masters")

// raw85 is 85.000 °C, the DS18B20's power-on register value.
const raw85 = 85000

// Source is the real reading.Source: DS18B20s through the kernel's w1 sysfs
// interface. It is the only code in tanktemp that knows about sysfs files,
// scratchpads or the sensor's quirks (docs/ONEWIRE.md).
type Source struct {
	FS       fs.FS // laid out like /sys/bus/w1/devices
	Attempts int   // reads per sensor before giving up; below 1 means 1

	// Logf, if set, is told about every read that will be (or was) retried:
	// a failed attempt, with its number and the error, and the 85 °C extra
	// read and what came of it. It's the bench detail behind log.verbose
	// (issue #2): without it, a read that recovered on its second attempt
	// leaves no trace, and one that never did shows only its final code.
	//
	// rom is its own argument, and msg never repeats it, so the caller can
	// name the probe however it likes: cmd/tanktemp turns it into the
	// tank/position from the config, which this package doesn't know.
	//
	// nil (the default) logs nothing. It's called from one goroutine per
	// bus, so it must be safe for concurrent use (log.Logger.Printf is). It
	// only reports what was read; it never changes or repeats a read.
	Logf func(rom, msg string)
}

// logf calls Logf, if it is set, with a formatted message about rom.
func (s *Source) logf(rom, format string, args ...any) {
	if s.Logf != nil {
		s.Logf(rom, fmt.Sprintf(format, args...))
	}
}

// errText is err's message without the "<rom>: " the handler's errors start
// with, since Logf already has the ROM.
func errText(rom string, err error) string {
	return strings.TrimPrefix(err.Error(), rom+": ")
}

// degrees formats raw millidegrees for a log line, as the kernel's t= does.
func degrees(milliC int) string { return fmt.Sprintf("%.3f °C", float64(milliC)/1000) }

// NewSource reads from dir, normally DevicesDir.
func NewSource(dir string, attempts int) *Source {
	return &Source{FS: os.DirFS(dir), Attempts: attempts}
}

var _ reading.Source = (*Source)(nil)

// Discover implements reading.Source. Junk devices (anything not a DS18B20,
// such as the fake 00-… IDs a noisy bus search invents) are left out.
func (s *Source) Discover(ctx context.Context) (reading.Topology, error) {
	sys := SysFS{FS: s.FS}
	masters, err := fs.Glob(s.FS, "w1_bus_master*")
	if err != nil {
		return reading.Topology{}, err
	}
	if len(masters) == 0 {
		return reading.Topology{}, ErrNoBuses
	}
	byBus, err := sys.Thermometers()
	if err != nil {
		return reading.Topology{}, err
	}
	return reading.NewTopology(byBus), nil
}

// Read implements reading.Source. Each bus is read in its own goroutine (the
// kernel serialises reads within a bus, and buses are independent: 2×5
// sensors take 4 s in parallel against 8 s in series). The context is
// checked before every read; a read in progress can't be interrupted, so a
// cancel takes effect within one conversion per bus (~0.8 s).
func (s *Source) Read(ctx context.Context, t reading.Topology, roms []string) map[string]reading.Reading {
	groups := map[string][]string{}
	for _, rom := range roms {
		bus, _ := t.Bus(rom)
		groups[bus] = append(groups[bus], rom)
	}
	buses := make([]string, 0, len(groups))
	for b := range groups {
		buses = append(buses, b)
	}
	sort.Strings(buses)

	out := make(map[string]reading.Reading, len(roms))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, bus := range buses {
		wg.Add(1)
		go func(bus string, roms []string) {
			defer wg.Done()
			for _, rom := range roms {
				r := s.readChecked(ctx, rom)
				r.Bus = bus
				mu.Lock()
				out[rom] = r
				mu.Unlock()
			}
		}(bus, groups[bus])
	}
	wg.Wait()
	return out
}

// readChecked reads rom until a reading passes every check, up to Attempts
// times. Retrying works because every read is a fresh conversion: a CRC
// glitch or a power-on reset is gone by the next read. A missing sensor is
// not retried, since that won't make it appear.
//
// 85.000 °C is special. It passes every check when the scratchpad's byte 6
// says it's a real conversion, and a hot water cylinder can genuinely reach
// it. But until a real power-on-reset scratchpad has been captured on this
// hardware, the byte 6 test rests on the datasheet alone. So an accepted
// 85.000 gets one extra read: a different good value replaces it; anything
// else keeps the 85.000, flagged reading.Suspect85, never dropped.
func (s *Source) readChecked(ctx context.Context, rom string) reading.Reading {
	attempts := s.Attempts
	if attempts < 1 {
		attempts = 1
	}
	start := time.Now()
	r := reading.Reading{ROM: rom}
	finish := func() reading.Reading {
		r.Took = time.Since(start)
		return r
	}
	for r.Attempts < attempts {
		if ctx.Err() != nil {
			r.Code, r.MilliC = reading.Cancelled, nil
			return finish()
		}
		r.Attempts++
		mc, err := s.readOnce(rom)
		if err == nil {
			r.Code, r.MilliC = reading.OK, &mc
			if mc == raw85 {
				s.recheck85(ctx, &r, attempts)
			}
			return finish()
		}
		r.Code = codeFor(err)
		if r.Code == reading.NotPresent {
			// Not retried, so not logged: the sweep's fault line names it.
			break
		}
		s.logf(rom, "attempt %d/%d: %s (%s)", r.Attempts, attempts, r.Code, errText(rom, err))
	}
	return finish()
}

// recheck85 is the extra read an accepted 85.000 °C gets. r.Attempts is the
// attempt that read it, out of attempts, for the log.
//
// Every log line that promises the extra read is followed by its outcome,
// and a shutdown that skips it says so on the same line (G32).
func (s *Source) recheck85(ctx context.Context, r *reading.Reading, attempts int) {
	if ctx.Err() != nil {
		s.logf(r.ROM, "attempt %d/%d: %s, extra read skipped (cancelled), flagged %s", r.Attempts, attempts, degrees(raw85), reading.Suspect85)
		r.Suspect = reading.Suspect85
		return
	}
	s.logf(r.ROM, "attempt %d/%d: %s, reading once more to check", r.Attempts, attempts, degrees(raw85))
	r.Attempts++
	mc, err := s.readOnce(r.ROM)
	switch {
	case err == nil && mc != raw85:
		s.logf(r.ROM, "extra read: %s, keeping it", degrees(mc))
		r.MilliC = &mc
		return
	case err == nil:
		s.logf(r.ROM, "extra read: %s again, flagged %s", degrees(mc), reading.Suspect85)
	default:
		s.logf(r.ROM, "extra read failed (%s: %s), keeping %s flagged %s", codeFor(err), errText(r.ROM, err), degrees(raw85), reading.Suspect85)
	}
	r.Suspect = reading.Suspect85
}

// readOnce reads and checks one conversion, returning raw millidegrees.
func (s *Source) readOnce(rom string) (int, error) {
	w, err := SysFS{FS: s.FS}.Read(rom)
	if err != nil {
		return 0, err
	}
	if err := w.Check(); err != nil {
		return 0, fmt.Errorf("%s: %w", rom, err)
	}
	return w.Scratchpad.MilliC(), nil
}

// codeFor maps the handler's typed errors to wire codes, in exactly one
// place. An error nobody has seen yet becomes read_error rather than being
// guessed at.
func codeFor(err error) reading.Code {
	switch {
	case err == nil:
		return reading.OK
	case errors.Is(err, ErrNotPresent):
		return reading.NotPresent
	case errors.Is(err, ErrBusLow):
		return reading.BusLow
	case errors.Is(err, ErrCRC):
		return reading.CRC
	case errors.Is(err, ErrPowerOnReset):
		return reading.PowerOnReset
	case errors.Is(err, ErrBadScratchpad):
		return reading.BadScratchpad
	case errors.Is(err, ErrOutOfRange):
		return reading.OutOfRange
	case errors.Is(err, ErrMalformed):
		return reading.Malformed
	}
	return reading.ReadError
}

// Inspect implements reading.Source: ext_power (1 = powered mode) and
// resolution (bits). A file that is missing or unparseable leaves that field
// nil (unknown) rather than guessing. A sensor not in sysfs is ErrNotPresent.
func (s *Source) Inspect(ctx context.Context, rom string) (reading.Info, error) {
	if _, err := fs.Stat(s.FS, rom); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return reading.Info{}, fmt.Errorf("%s: %w", rom, ErrNotPresent)
		}
		return reading.Info{}, err
	}
	var info reading.Info
	if v, ok := s.readInt(rom + "/" + fileExtPower); ok && (v == 0 || v == 1) {
		b := v == 1
		info.ExtPower = &b
	}
	if v, ok := s.readInt(rom + "/" + fileResolution); ok {
		info.Resolution = &v
	}
	return info, nil
}

func (s *Source) readInt(name string) (int, bool) {
	data, err := fs.ReadFile(s.FS, name)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return v, err == nil
}
