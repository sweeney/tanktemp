// Package sweep turns the config and a reading.Source into one sweep: every
// configured sensor of every tank, in order, calibrated, with its quality.
// It is pure business logic: the sensors come through reading.Source and
// time through injected functions, so it is tested with readingtest.Fake.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
)

// ErrCancelled: the sweep was cancelled (shutdown) part-way. Its readings
// are incomplete and must not be published.
var ErrCancelled = errors.New("sweep cancelled")

// Sweeper reads every configured sensor.
type Sweeper struct {
	Tanks  []config.Tank // sensors sorted by order, as config.Load returns them
	Source reading.Source
	Now    func() time.Time
	Synced func() bool // whether the system clock is NTP-synced
}

// Sweep is one complete read of every configured sensor.
type Sweep struct {
	At            time.Time     // when the sweep started (UTC)
	Duration      time.Duration // how long it took
	ClockSynced   bool
	Tanks         []Tank   // in config order
	Unexpected    []string // thermometers on a bus but not in config, sorted
	DiscoverError string   // set if discovery failed; every sensor then reads not_present
}

// Tank is one tank's readings.
type Tank struct {
	DeviceID string
	Sensors  []Sensor // in order: 1 = bottom
}

// Sensor is one probe's outcome: the config facts, the raw and calibrated
// values (nil unless Code is OK), and its quality.
type Sensor struct {
	Position string
	Order    int
	HeightMM *int
	ROM      string
	OffsetC  float64
	RawC     *float64
	TempC    *float64 // RawC + OffsetC
	Code     reading.Code
	Suspect  string
	Attempts int
	Bus      string // "" when not on any bus
}

// Counts returns how many sensors read OK and how many failed.
func (s Sweep) Counts() (ok, failed int) {
	for _, t := range s.Tanks {
		for _, x := range t.Sensors {
			if x.Code == reading.OK {
				ok++
			} else {
				failed++
			}
		}
	}
	return ok, failed
}

// Faults names every probe that failed or read a suspect value, grouped by
// code in the order first seen, as "code: tank/position, …; code: …". It is
// empty for a clean sweep.
func (s Sweep) Faults() string {
	var order []string
	byCode := map[string][]string{}
	add := func(code, where string) {
		if _, seen := byCode[code]; !seen {
			order = append(order, code)
		}
		byCode[code] = append(byCode[code], where)
	}
	for _, t := range s.Tanks {
		for _, x := range t.Sensors {
			where := t.DeviceID + "/" + x.Position
			switch {
			case x.Code != reading.OK:
				add(string(x.Code), where)
			case x.Suspect != "":
				add(x.Suspect, where)
			}
		}
	}
	parts := make([]string, 0, len(order))
	for _, code := range order {
		parts = append(parts, code+": "+strings.Join(byCode[code], ", "))
	}
	return strings.Join(parts, "; ")
}

// Line is the verbose log's one line for this sweep (issue #2, log.verbose):
//
//	sweep 4.0s 9/10 left 36.2 58.4 null 66.1 65.2 | right 39.9 53.3 54.1 49.6 57.2
//
// That is the duration, OK/total probes, then each tank's device_id and its
// calibrated temperatures to 0.1 °C, bottom (order 1) to top, tanks
// separated by " | ". A probe without a value is "null" in its place, never
// 0 and never left out, so the columns line up sweep after sweep. Suspect
// values are shown (they are published); Faults says what went wrong.
//
// It is one line for the whole sweep, starting "sweep " (the fault line
// starts "sweep:"), so `journalctl -u tanktemp | grep '^sweep '` gives the
// readings over time. It is built only from s: logging it re-reads nothing
// and can't change what was read or when.
func (s Sweep) Line() string {
	ok, failed := s.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "sweep %.1fs %d/%d", s.Duration.Seconds(), ok, ok+failed)
	for i, t := range s.Tanks {
		if i > 0 {
			b.WriteString(" |")
		}
		b.WriteString(" " + t.DeviceID)
		for _, x := range t.Sensors {
			if x.Code == reading.OK && x.TempC != nil {
				fmt.Fprintf(&b, " %.1f", *x.TempC)
			} else {
				b.WriteString(" null")
			}
		}
	}
	return b.String()
}

// Sweep reads every configured sensor once (with the Source's retries).
func (s *Sweeper) Sweep(ctx context.Context) (Sweep, error) {
	start := s.Now().UTC()
	out := Sweep{At: start, ClockSynced: s.Synced()}

	top, err := s.Source.Discover(ctx)
	if err != nil {
		out.DiscoverError = err.Error()
		top = reading.Topology{}
	}

	configured := map[string]bool{}
	var roms []string
	for _, t := range s.Tanks {
		for _, x := range t.Sensors {
			configured[x.ROM] = true
			roms = append(roms, x.ROM)
		}
	}
	readings := s.Source.Read(ctx, top, roms)

	for _, t := range s.Tanks {
		tank := Tank{DeviceID: t.DeviceID}
		for _, x := range t.Sensors {
			r := readings[x.ROM]
			if r.Code == reading.Cancelled || ctx.Err() != nil {
				return Sweep{}, ErrCancelled
			}
			tank.Sensors = append(tank.Sensors, sensorFrom(x, r))
		}
		out.Tanks = append(out.Tanks, tank)
	}
	for _, rom := range top.ROMs() {
		if !configured[rom] {
			out.Unexpected = append(out.Unexpected, rom)
		}
	}
	sort.Strings(out.Unexpected)
	out.Duration = s.Now().UTC().Sub(start)
	return out, nil
}

func sensorFrom(c config.Sensor, r reading.Reading) Sensor {
	s := Sensor{
		Position: c.Position,
		Order:    c.Order,
		HeightMM: c.HeightMM,
		ROM:      c.ROM,
		OffsetC:  c.OffsetC,
		Code:     r.Code,
		Suspect:  r.Suspect,
		Attempts: r.Attempts,
		Bus:      r.Bus,
	}
	if s.Code == "" {
		s.Code = reading.ReadError // the Source broke its contract; say so
	}
	if r.Code == reading.OK && r.MilliC != nil {
		raw := rawC(*r.MilliC)
		t := calibrate(*r.MilliC, c.OffsetC)
		s.RawC, s.TempC = &raw, &t
	}
	return s
}

// calibrate applies the offset and rounds to 4 decimals, so a published
// value never carries float noise (61.375 - 0.19 is 61.185, not
// 61.18499999999999).
// rawC recovers the register's exact value. A Source reports millidegrees
// as the kernel computes them, raw*1000/16 truncated toward zero, which
// drops the half millidegree of every odd sixteenth (12.0625 °C arrives as
// 12062). Rounding back to the nearest sixteenth is exact for every value
// the DS18B20 can report.
func rawC(milliC int) float64 { return math.Round(float64(milliC)*16/1000) / 16 }

func calibrate(milliC int, offsetC float64) float64 {
	return math.Round((rawC(milliC)+offsetC)*1e4) / 1e4
}
