//go:build !linux

package gpioedge

import (
	"errors"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
)

// line is never made off Linux; it exists so Open compiles everywhere.
type line struct{ meter.Source }

// The GPIO character device is Linux's; elsewhere (a developer's Mac)
// requesting a line always fails, and tanktemp runs without its meter.
func request(chip string, offset int, debounce time.Duration) (*line, error) {
	return nil, errors.New("GPIO lines need Linux")
}
