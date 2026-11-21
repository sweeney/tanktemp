//go:build !linux

package gpioedge

import (
	"errors"
	"time"
)

// The GPIO character device is Linux's; elsewhere (a developer's Mac)
// requesting a line always fails, and tanktemp runs without its meter.
func request(Settings, func(time.Duration, bool, uint32)) (chipLine, error) {
	return nil, errors.New("GPIO lines need Linux")
}

var start = time.Now()

// Now, off Linux, is time since the process started: there are no kernel
// edges to match, only the tests' clock to keep monotonic.
func Now() time.Duration { return time.Since(start) }
