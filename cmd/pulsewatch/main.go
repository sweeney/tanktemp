// Command pulsewatch watches the water meter's GPIO line on the Pi and says
// what tanktemp's counter makes of each edge: a litre counted (and how long
// the reed was closed), noise, a short closing, a glitch, or edges lost.
// It is the bench tool for the bucket test and for tuning the debounce,
// hold and min interval (docs/WATER-METER-PLAN.md), and runs the same
// gpioedge source and meter.Counter as tanktemp, with the same 50 ms tick.
//
//	pulsewatch                          # GPIO 27 on gpiochip0, the config's defaults
//	pulsewatch -for 2m -debounce 10ms   # stop after 2 minutes
//
// The line must be free: stop tanktemp (and anything else holding it)
// first. Read-only: it only watches the line.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/gpioedge"
	"github.com/sweeney/tanktemp/internal/meter"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("pulsewatch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	d := config.DefaultMeter
	chip := fs.String("chip", d.Chip, "the GPIO chip")
	gpio := fs.Int("gpio", 27, "the BCM line")
	debounce := fs.Duration("debounce", d.Debounce, "the kernel debounce (at least 5ms)")
	hold := fs.Duration("hold", d.Hold, "how long a closing must hold to count")
	minInterval := fs.Duration("min-interval", d.MinInterval, "the shortest gap between litres")
	forD := fs.Duration("for", 0, "stop after this long (0: at Ctrl-C)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	src, err := gpioedge.Open(*chip, *gpio, *debounce)
	if err != nil {
		fmt.Fprintf(errOut, "pulsewatch: %v\n", err)
		return 1
	}
	defer src.Close()
	k, err := meter.New(meter.Config{Hold: *hold, MinInterval: *minInterval}, src.Initial())
	if err != nil {
		fmt.Fprintf(errOut, "pulsewatch: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *forD > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forD)
		defer cancel()
	}
	fmt.Fprintf(out, "GPIO %d on %s: %v; debounce %v, hold %v, min interval %v\n", *gpio, *chip, src.Initial(), *debounce, *hold, *minInterval)
	ticks := make(chan time.Duration)
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case ticks <- gpioedge.Now():
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	watch(ctx, src, ticks, k, out)
	return 0
}

// watch prints a line per edge (and per litre a tick confirms) until ctx
// ends or the line closes, then a summary.
func watch(ctx context.Context, src meter.Source, ticks <-chan time.Duration, k *meter.Counter, out io.Writer) {
	var prev, closing time.Duration
	first := true
	defer func() {
		s := k.Snapshot()
		fmt.Fprintf(out, "%d litres; noise %d, short %d, glitches %d, lost %d\n", s.Count, s.Noise, s.Short, s.Glitches, s.Lost)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			before := k.Snapshot()
			for _, ev := range k.Tick(now) {
				if ev.Kind == meter.Pulse {
					fmt.Fprintf(out, "tick           litre %d (held %v, resting on the magnet)\n", ev.Snap.Count, now-ev.At)
				}
			}
			if k.Snapshot().Glitches > before.Glitches {
				fmt.Fprintf(out, "tick           glitch: under the min interval, not counted\n")
			}
		case e, ok := <-src.Edges():
			if !ok {
				return
			}
			dt := e.At - prev
			if first {
				dt, first = 0, false
			}
			prev = e.At
			before := k.Snapshot()
			evs := k.Observe(e)
			after := k.Snapshot()
			var says []string
			if n := after.Lost - before.Lost; n > 0 {
				says = append(says, fmt.Sprintf("%d lost before it", n))
			}
			if after.Noise > before.Noise {
				says = append(says, fmt.Sprintf("noise: already %v", e.Level))
			}
			if after.Short > before.Short {
				says = append(says, fmt.Sprintf("short: reopened in %v, under the hold", e.At-closing))
			}
			if after.Glitches > before.Glitches {
				says = append(says, "glitch: under the min interval, not counted")
			}
			for _, ev := range evs {
				switch ev.Kind {
				case meter.Pulse:
					says = append(says, fmt.Sprintf("litre %d", ev.Snap.Count))
				case meter.Opened:
					says = append(says, fmt.Sprintf("closed %v", *ev.Snap.LastClosed))
				}
			}
			if e.Level == meter.Closed && after.Noise == before.Noise && after.Lost == before.Lost {
				closing = e.At
				if len(says) == 0 {
					says = append(says, "held")
				}
			}
			fmt.Fprintf(out, "seq %d +%.3fs %-6v %s\n", e.Seqno, dt.Seconds(), e.Level, strings.Join(says, "; "))
		}
	}
}
