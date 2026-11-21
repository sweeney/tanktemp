// Command pulsewatch watches the water meter's GPIO line on the Pi and says
// what tanktemp's counter makes of each edge: a litre counted (and how long
// the reed was closed), noise, a short spike, a glitch, or edges lost. It is
// the bench tool for the bucket test and for tuning the debounce, hold and
// min interval (docs/METER.md). It runs the same gpioedge line, the same
// meter.Loop and Counter, and the same 50 ms tick as tanktemp, so what it
// says is what tanktemp would count.
//
// The line and its settings come from tanktemp's config (the pin is never
// built in); a flag overrides one value:
//
//	pulsewatch                                  # the meter in /etc/tanktemp/config.yaml
//	pulsewatch -config my.yaml -meter hot_water_meter -for 2m
//	pulsewatch -debounce 10ms                   # the config's meter, but a 10ms debounce
//	pulsewatch -config "" -gpio 27              # no config: the defaults, on GPIO 27
//
// The line must be free: stop tanktemp (and anything else holding it)
// first. The config is root:tanktemp 0640, so either run it as a user who
// can read it or pass a copy with -config. Read-only: it only watches.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/gpioedge"
	"github.com/sweeney/tanktemp/internal/meter"
)

// tick is tanktemp's MeterTick (GUESSES G59).
const tick = 50 * time.Millisecond

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, func(s gpioedge.Settings) (meter.Source, error) {
		return gpioedge.Open(s)
	}))
}

// opener requests the line: gpioedge.Open on the Pi.
type opener func(gpioedge.Settings) (meter.Source, error)

func run(args []string, out, errOut io.Writer, open opener) int {
	fs := flag.NewFlagSet("pulsewatch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfgPath := fs.String("config", "/etc/tanktemp/config.yaml", `tanktemp's config, for the meter's line and settings ("" for none)`)
	id := fs.String("meter", "", "the meter's device_id (default: the config's only meter)")
	d := config.DefaultMeter
	chip := fs.String("chip", d.Chip, "the GPIO chip")
	gpio := fs.Int("gpio", -1, "the BCM line")
	bias := fs.String("bias", d.Bias, "pull-up, pull-down or none")
	closedLevel := fs.String("closed-level", d.ClosedLevel, "low or high: what the line reads with the reed closed")
	debounce := fs.Duration("debounce", d.Debounce, "the kernel debounce (at least 5ms)")
	hold := fs.Duration("hold", d.Hold, "how long a change must hold to count")
	minInterval := fs.Duration("min-interval", d.MinInterval, "the shortest gap between litres")
	forD := fs.Duration("for", 0, "stop after this long (0: at Ctrl-C)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	m, from, err := meterFrom(*cfgPath, *id)
	if err != nil {
		fmt.Fprintf(errOut, "pulsewatch: %v\n", err)
		return 2
	}
	// The config's values, then any flag given.
	for name, apply := range map[string]func(){
		"chip": func() { m.Chip = *chip }, "gpio": func() { m.GPIO = gpio },
		"bias": func() { m.Bias = *bias }, "closed-level": func() { m.ClosedLevel = *closedLevel },
		"debounce": func() { m.Debounce = *debounce }, "hold": func() { m.Hold = *hold },
		"min-interval": func() { m.MinInterval = *minInterval },
	} {
		if set[name] {
			apply()
		}
	}
	if m.GPIO == nil || *m.GPIO < 0 {
		fmt.Fprintf(errOut, "pulsewatch: no line to watch: %s names no meter, so give -gpio\n", from)
		return 2
	}
	s, err := settings(m)
	if err != nil {
		fmt.Fprintf(errOut, "pulsewatch: %v\n", err)
		return 2
	}
	c := meter.Config{Hold: m.Hold, MinInterval: m.MinInterval}
	if _, err := meter.New(c, meter.Open); err != nil {
		fmt.Fprintf(errOut, "pulsewatch: %v\n", err)
		return 2
	}
	src, err := open(s)
	if err != nil {
		fmt.Fprintf(errOut, "pulsewatch: %v\n", err)
		return 1
	}
	defer src.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *forD > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *forD)
		defer cancel()
	}
	fmt.Fprintf(out, "%s: GPIO %d on %s, %s, closed reads %s: now %v; debounce %v, hold %v, min interval %v\n",
		from, s.Offset, s.Chip, m.Bias, m.ClosedLevel, src.Initial(), m.Debounce, m.Hold, m.MinInterval)
	ticks := make(chan time.Duration)
	go func() {
		t := time.NewTicker(tick)
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
	watch(ctx, src, ticks, c, out)
	return 0
}

// meterFrom is the meter to watch, and where it came from: the config's
// meter named id (or its only one), or the defaults with no line when
// there's no config.
func meterFrom(path, id string) (config.Meter, string, error) {
	none := config.DefaultMeter
	if path == "" {
		return none, "no config", nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return none, path + " (not found)", nil
	}
	if err != nil {
		return none, "", fmt.Errorf("%v (it is root:tanktemp 0640: run as a user who can read it, or pass a copy with -config)", err)
	}
	cfg, err := config.Load(b)
	if err != nil {
		return none, "", fmt.Errorf("%s: %w", path, err)
	}
	for _, m := range cfg.Meters {
		if id == "" || m.DeviceID == id {
			return m, fmt.Sprintf("%s, meter %s", path, m.DeviceID), nil
		}
	}
	if id != "" {
		return none, "", fmt.Errorf("%s has no meter %q", path, id)
	}
	return none, path, nil
}

// settings is the meter's line as gpioedge requests it.
func settings(m config.Meter) (gpioedge.Settings, error) {
	b, ok := map[string]gpioedge.Bias{config.BiasPullUp: gpioedge.PullUp, config.BiasPullDown: gpioedge.PullDown, config.BiasNone: gpioedge.None}[m.Bias]
	if !ok {
		return gpioedge.Settings{}, fmt.Errorf("bias %q: pull-up, pull-down or none", m.Bias)
	}
	if m.ClosedLevel != config.ClosedLow && m.ClosedLevel != config.ClosedHigh {
		return gpioedge.Settings{}, fmt.Errorf("closed level %q: low or high", m.ClosedLevel)
	}
	return gpioedge.Settings{Chip: m.Chip, Offset: *m.GPIO, Debounce: m.Debounce, Bias: b, ClosedHigh: m.ClosedLevel == config.ClosedHigh}, nil
}

// watch runs a meter.Loop over src (as tanktemp does), printing a line per
// edge, and per tick that does something, until ctx ends or the line
// closes; then a summary.
func watch(ctx context.Context, src meter.Source, ticks <-chan time.Duration, c meter.Config, out io.Writer) {
	k, err := meter.New(c, src.Initial())
	if err != nil {
		fmt.Fprintf(out, "%v\n", err)
		return
	}
	l := meter.NewLoop(k, src, ticks, func(meter.Event) {})
	var prev, changed time.Duration
	first := true
	last := k.Snapshot()
	l.Trace = func(st meter.Step) {
		last = st.After
		b, a := st.Before, st.After
		var says []string
		if a.Glitches > b.Glitches {
			says = append(says, "glitch: under the min interval, not counted")
		}
		for _, ev := range st.Events {
			switch ev.Kind {
			case meter.Pulse:
				if st.Edge == nil {
					says = append(says, fmt.Sprintf("litre %d (confirmed by a tick, %v after its closing)", ev.Snap.Count, st.Now-ev.At))
				} else {
					says = append(says, fmt.Sprintf("litre %d", ev.Snap.Count))
				}
			case meter.Opened:
				says = append(says, fmt.Sprintf("litre %d ended: closed %v", ev.Snap.Count, *ev.Snap.LastClosed))
			}
		}
		e := st.Edge
		if e == nil {
			if len(says) > 0 {
				fmt.Fprintf(out, "tick           %s\n", strings.Join(says, "; "))
			}
			return
		}
		dt := e.At - prev
		if first {
			dt, first = 0, false
		}
		prev = e.At
		var pre []string
		if n := a.Lost - b.Lost; n > 0 {
			pre = append(pre, fmt.Sprintf("%d lost before it", n))
		}
		switch {
		case a.Noise > b.Noise:
			pre = append(pre, fmt.Sprintf("noise: already %v", e.Level))
		case a.Short > b.Short:
			pre = append(pre, fmt.Sprintf("short: back in %v, under the hold", e.At-changed))
		case a.Lost == b.Lost:
			changed = e.At
			if len(says) == 0 {
				pre = append(pre, "held")
			}
		}
		fmt.Fprintf(out, "seq %d +%.3fs %-6v %s\n", e.Seqno, dt.Seconds(), e.Level, strings.Join(append(pre, says...), "; "))
	}
	if err := l.Run(ctx); err != nil && !errors.Is(err, meter.ErrSourceClosed) {
		fmt.Fprintf(out, "%v\n", err)
	}
	fmt.Fprintf(out, "%d litres; noise %d, short %d, glitches %d, lost %d\n", last.Count, last.Noise, last.Short, last.Glitches, last.Lost)
}
