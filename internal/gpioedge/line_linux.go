//go:build linux

package gpioedge

import (
	"time"

	"github.com/warthog618/go-gpiocdev"
	"golang.org/x/sys/unix"
)

// request is the requester on Linux: the only code that touches
// go-gpiocdev.
func request(s Settings, h func(at time.Duration, high bool, seq uint32)) (chipLine, error) {
	bias := map[Bias]gpiocdev.LineReqOption{PullUp: gpiocdev.WithPullUp, PullDown: gpiocdev.WithPullDown, None: gpiocdev.WithBiasDisabled}[s.Bias]
	return gpiocdev.RequestLine(s.Chip, s.Offset,
		gpiocdev.WithConsumer("tanktemp-meter"),
		gpiocdev.AsInput,
		bias,
		gpiocdev.WithBothEdges,
		gpiocdev.WithDebounce(s.Debounce),
		gpiocdev.WithMonotonicEventClock,
		gpiocdev.WithEventHandler(func(e gpiocdev.LineEvent) {
			h(e.Timestamp, e.Type == gpiocdev.LineEventRisingEdge, e.LineSeqno)
		}))
}

// Now is CLOCK_MONOTONIC, the clock the kernel stamps edges with
// (WithMonotonicEventClock): the meter turns an edge's time into wall time
// against it.
func Now() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(err) // CLOCK_MONOTONIC always exists on Linux
	}
	return time.Duration(ts.Nano())
}
