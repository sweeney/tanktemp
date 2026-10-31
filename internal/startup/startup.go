// Package startup runs the once-at-boot checks from docs/ONEWIRE.md: every
// configured sensor present, in powered mode, at 12-bit; the buses there at
// all; and the clock synced. Problems are reported (on /healthz and in the
// status message), never fatal: a missing probe must not stop the others
// being published.
package startup

import (
	"context"
	"fmt"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/reading"
)

// Problem codes. Part of the status payload.
const (
	CodeDiscoverFailed = "discover_failed"
	CodeNotPresent     = "not_present"
	CodeParasitePower  = "parasite_power"
	CodeResolution     = "resolution"
	CodeInspectFailed  = "inspect_failed"
)

// WantResolution is the resolution tanktemp expects (12-bit, 0.0625 °C).
const WantResolution = 12

// Problem is one startup finding. ROM is empty for a bus-wide problem.
type Problem struct {
	ROM     string
	Code    string
	Message string
}

// Report is the outcome of the startup checks.
type Report struct {
	Problems []Problem
}

// OK reports whether nothing was found.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Check inspects every configured sensor, in config order.
func Check(ctx context.Context, tanks []config.Tank, src reading.Source) Report {
	var r Report
	top, err := src.Discover(ctx)
	if err != nil {
		r.Problems = append(r.Problems, Problem{Code: CodeDiscoverFailed, Message: err.Error()})
		return r
	}
	for _, t := range tanks {
		for _, s := range t.Sensors {
			if _, ok := top.Bus(s.ROM); !ok {
				r.Problems = append(r.Problems, Problem{ROM: s.ROM, Code: CodeNotPresent,
					Message: fmt.Sprintf("%s (%s %s) is on no bus", s.ROM, t.DeviceID, s.Position)})
				continue
			}
			info, err := src.Inspect(ctx, s.ROM)
			if err != nil {
				r.Problems = append(r.Problems, Problem{ROM: s.ROM, Code: CodeInspectFailed, Message: err.Error()})
				continue
			}
			if info.ExtPower != nil && !*info.ExtPower {
				r.Problems = append(r.Problems, Problem{ROM: s.ROM, Code: CodeParasitePower,
					Message: fmt.Sprintf("%s is in parasite-power mode; wire VDD to 3.3 V", s.ROM)})
			}
			if info.Resolution != nil && *info.Resolution != WantResolution {
				r.Problems = append(r.Problems, Problem{ROM: s.ROM, Code: CodeResolution,
					Message: fmt.Sprintf("%s is at %d-bit, want %d", s.ROM, *info.Resolution, WantResolution)})
			}
		}
	}
	return r
}

// WaitForSync polls synced every poll until it is true, wait has passed,
// or ctx is done, and returns whether the clock is synced. The Pi has no
// real-time clock, so until NTP syncs, timestamps can run backwards.
func WaitForSync(ctx context.Context, synced func() bool, wait, poll time.Duration) bool {
	if synced() {
		return true
	}
	if wait <= 0 {
		return false
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return synced()
		case <-tick.C:
			if synced() {
				return true
			}
		}
	}
}
