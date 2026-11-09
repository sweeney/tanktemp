// Command tanktemp reads DS18B20 probes on hot water cylinders and publishes
// each sweep to MQTT, with /healthz and a status page over HTTP.
//
//	tanktemp -config /etc/tanktemp/config.yaml
//	tanktemp -config /etc/tanktemp/config.yaml -check-config
//	tanktemp -version
//
// This file only wires things together: the logic lives in internal/ and is
// tested there, and run is tested with every outside dependency faked.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sweeney/common/health/sysinfo"
	"github.com/sweeney/common/lifecycle"
	"github.com/sweeney/common/mqttpub"
	"github.com/sweeney/common/mqttpub/retry"
	"github.com/sweeney/tanktemp/internal/cadence"
	"github.com/sweeney/tanktemp/internal/config"
	"github.com/sweeney/tanktemp/internal/onewire"
	"github.com/sweeney/tanktemp/internal/payload"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/startup"
	"github.com/sweeney/tanktemp/internal/status"
	"github.com/sweeney/tanktemp/internal/sweep"
	"github.com/sweeney/tanktemp/internal/web"
)

// version is stamped at build time: -ldflags "-X main.version=<commit>".
var version = "dev"

// SyncedFlag exists once systemd-timesyncd has synced the clock.
const SyncedFlag = "/run/systemd/timesync/synchronized"

// ConnectWait is how long startup waits for the broker before sweeping
// anyway, so STARTUP normally reaches it but a dead broker can't stop the
// sensors being read and served over HTTP.
const ConnectWait = 10 * time.Second

func main() { os.Exit(realMain(os.Args[1:], os.Stderr)) }

func realMain(args []string, out io.Writer) int {
	fl := flag.NewFlagSet("tanktemp", flag.ContinueOnError)
	fl.SetOutput(out)
	path := fl.String("config", "/etc/tanktemp/config.yaml", "config file")
	check := fl.Bool("check-config", false, "validate the config and exit")
	showVersion := fl.Bool("version", false, "print the version and exit")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(out, version)
		return 0
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintf(out, "tanktemp: %v\n", err)
		return 1
	}
	cfg, err := config.Load(data)
	if err != nil {
		fmt.Fprintf(out, "tanktemp: %s: %v\n", *path, err)
		return 1
	}
	if *check {
		n := 0
		for _, t := range cfg.Tanks {
			n += len(t.Sensors)
		}
		// The verbose state sits before "publishing to": deploy/lib.sh's
		// parse_check_config reads the prefix and broker from the end of
		// this line. "ON" is in capitals because it's the one to notice.
		verbose := "off"
		if cfg.Log.Verbose {
			verbose = "ON"
		}
		fmt.Fprintf(out, "%s: ok: %d tank%s, %d sensor%s, %s, verbose log %s, publishing to %s on %s\n",
			*path, len(cfg.Tanks), plural(len(cfg.Tanks)), n, plural(n), cadenceSummary(cfg.Sweep), verbose, cfg.MQTT.TopicPrefix, cfg.MQTT.Broker)
		return 0
	}
	if err := serve(cfg); err != nil {
		log.Printf("tanktemp: %v", err)
		return 1
	}
	return 0
}

// cadenceSummary is -check-config's account of the cadence (issue #1), so a
// deploy's --check shows whether sweep.fast is on and with what:
//
//	sweep every 30s
//	sweep every 30s, 10s while a probe changes 1.5 °C/min (hold 2m, decay 5m)
func cadenceSummary(s config.Sweep) string {
	out := "sweep every " + shortDuration(s.Interval)
	if f := s.Fast; f != nil {
		out += fmt.Sprintf(", %s while a probe changes %v °C/min (hold %s, decay %s)",
			shortDuration(f.Interval), f.TriggerCPerMin, shortDuration(f.Hold), shortDuration(f.Decay))
	}
	return out
}

// shortDuration is d as a config file would say it: "2m", not "2m0s", and
// "1h", not "1h0m0s".
func shortDuration(d time.Duration) string {
	str := d.String()
	if strings.HasSuffix(str, "m0s") {
		str = strings.TrimSuffix(str, "0s")
	}
	if strings.HasSuffix(str, "h0m") {
		str = strings.TrimSuffix(str, "0m")
	}
	return str
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// serve builds the real dependencies and runs.
func serve(cfg config.Config) error {
	logger := log.New(os.Stderr, "", 0) // journald adds timestamps
	if cfg.MQTT.ClientID == "" {
		host, _ := os.Hostname()
		cfg.MQTT.ClientID = "tanktemp-" + host
	}
	password := ""
	if cfg.MQTT.PasswordFile != "" {
		b, err := os.ReadFile(cfg.MQTT.PasswordFile)
		if err != nil {
			return fmt.Errorf("mqtt password file: %w", err)
		}
		password = strings.TrimSpace(string(b))
	}
	pub, err := mqttpub.NewMQTTPublisher(mqttpub.Options{
		Broker: cfg.MQTT.Broker, ClientID: cfg.MQTT.ClientID,
		Username: cfg.MQTT.Username, Password: password, WillQOS: 1,
	}, payload.StatusTopic(cfg.MQTT.TopicPrefix), string(payload.LWT()))
	if err != nil {
		return err
	}
	defer pub.Close()

	ln, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connected := make(chan struct{})
	go func() {
		_, err := retry.Connect(ctx, "MQTT broker "+cfg.MQTT.Broker, mqttpub.DefaultTuning().Reconnect,
			func() error { return pub.Connect(ctx) }, retry.Sleep)
		if err == nil {
			logger.Printf("connected to %s as %s", cfg.MQTT.Broker, cfg.MQTT.ClientID)
			close(connected)
		}
	}()
	select {
	case <-connected:
	case <-time.After(ConnectWait):
		logger.Printf("MQTT not connected after %v; sweeping anyway", ConnectWait)
	}

	// log.verbose is turned into a logger here, once. run's sweep and
	// cadence lines and the 1-Wire source's retry lines all go to it.
	vlog := verboseLogger(logger, cfg.Log.Verbose)
	src := onewire.NewSource(cfg.W1Root, cfg.Sweep.Attempts)
	src.Logf = retryLogger(vlog, cfg.Tanks)

	return run(ctx, cfg, deps{
		Source:      src,
		Pub:         pub,
		Reconnected: pub.Reconnected(),
		Synced:      func() bool { _, err := os.Stat(SyncedFlag); return err == nil },
		Now:         time.Now,
		Listener:    ln,
		SysFS:       os.DirFS("/"),
		Disk:        sysinfo.StatfsDisk("/"),
		Logger:      logger,
		Verbose:     vlog,
		Version:     version,
	})
}

// publisher is what run needs from MQTT.
type publisher interface {
	Publish(mqttpub.Message) error
	Connected() bool
}

// deps is everything run takes from the outside world. Tests fake all of it.
type deps struct {
	Source      reading.Source
	Pub         publisher
	Reconnected <-chan struct{} // each MQTT reconnect; nil for none
	Synced      func() bool
	Now         func() time.Time

	After func(time.Duration) <-chan time.Time // the scheduler's timer; nil: time.After
	// Tick is a test hook: a fixed source of ticks, bypassing the adaptive
	// cadence's scheduler. nil (production): the scheduler, waiting
	// whatever internal/cadence decides after each sweep, using After.
	Tick          <-chan time.Time
	HeartbeatTick <-chan time.Time // nil: every cfg.Heartbeat
	Signals       <-chan os.Signal // nil: SIGINT and SIGTERM

	Listener net.Listener // nil: no HTTP
	SysFS    fs.FS
	Disk     func() (sysinfo.Disk, error)

	Logger  *log.Logger
	Verbose *log.Logger // bench-only lines (log.verbose); nil: discarded
	Version string
}

// verboseLogger is where the bench-only lines go (issue #2, log.verbose):
// the normal logger when verbose is on, a discard writer when it is off.
// serve calls it once and hands the result to everything that logs verbose
// detail (deps.Verbose, the 1-Wire source), so the switch lives in one
// place. That code just prints to it and never checks the switch itself,
// so turning it on adds lines and changes nothing else. The adaptive
// cadence's lines (#1) go to the same logger.
func verboseLogger(l *log.Logger, verbose bool) *log.Logger {
	if verbose && l != nil {
		return l
	}
	return log.New(io.Discard, "", 0)
}

// retryLogger is the 1-Wire source's Logf: it names each retried read by
// tank/position as well as ROM, so the journal reads at the bench without
// the config open beside it.
//
//	read hot-water-cylinder/bottom 28-000011721ce9 attempt 1/3: crc (scratchpad CRC mismatch)
//
// onewire never learns the config; this map is the only link. A ROM that
// isn't in the config (onewire reads only configured ones, so it shouldn't
// happen) is named by ROM alone.
func retryLogger(vlog *log.Logger, tanks []config.Tank) func(rom, msg string) {
	where := map[string]string{}
	for _, t := range tanks {
		for _, x := range t.Sensors {
			where[x.ROM] = t.DeviceID + "/" + x.Position + " "
		}
	}
	return func(rom, msg string) { vlog.Printf("read %s%s %s", where[rom], rom, msg) }
}

func eventName(k lifecycle.Kind) string {
	switch k {
	case lifecycle.Startup:
		return payload.EventStartup
	case lifecycle.Shutdown:
		return payload.EventShutdown
	}
	return payload.EventHeartbeat // Ready is the first heartbeat (docs/WIRE.md)
}

// run is tanktemp: startup checks, then sweeps on a schedule, publishing
// each tank's state and the lifecycle, until a signal.
func run(ctx context.Context, cfg config.Config, d deps) error {
	start := d.Now()
	vlog := d.Verbose
	if vlog == nil {
		vlog = verboseLogger(nil, false)
	}
	var ids []string
	for _, t := range cfg.Tanks {
		ids = append(ids, t.DeviceID)
	}
	// The adaptive cadence (issue #1). With no sweep.fast it decides the
	// slow interval every time, so there's one code path either way.
	cad := cadence.New(cadence.FromConfig(cfg.Sweep))
	tracker := status.New(status.Options{
		Start: start, Version: d.Version, Tanks: ids,
		// Interval stays the slow one: staleness is measured in slow
		// intervals whatever the cadence does (WIRE.md, Cadence).
		Interval: cfg.Sweep.Interval, Connected: d.Pub.Connected,
		Verbose: cfg.Log.Verbose, Cadence: cad.Status(),
	})
	prefix := cfg.MQTT.TopicPrefix

	if d.Listener != nil {
		srv := &http.Server{
			Handler:           (&web.Server{Tracker: tracker, Now: d.Now, SysFS: d.SysFS, Disk: d.Disk, Prefix: prefix}).Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := srv.Serve(d.Listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				d.Logger.Printf("http: %v", err)
			}
		}()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
	}

	synced := startup.WaitForSync(ctx, d.Synced, cfg.ClockSyncWait, time.Second)
	if !synced {
		d.Logger.Printf("clock not NTP-synced after %v; sweeps will say clock_synced:false", cfg.ClockSyncWait)
	}
	report := startup.Check(ctx, cfg.Tanks, d.Source)
	for _, p := range report.Problems {
		d.Logger.Printf("startup: %s", p.Message)
	}
	tracker.SetStartup(report)

	publishStatus := func(event string, at time.Time, reason string) error {
		snap := tracker.Snapshot()
		return d.Pub.Publish(mqttpub.Message{
			Topic: payload.StatusTopic(prefix),
			Payload: string(payload.Status(payload.StatusInput{
				Event: event, At: at, Version: snap.Version, Ready: snap.Ready,
				Uptime: at.Sub(start), Tanks: snap.Tanks, Last: snap.Last,
				Problems: snap.Problems, Reason: reason,
			})),
			QOS: 1, Retained: true,
		})
	}

	// After a reconnect the broker may hold our last will: replace it.
	if d.Reconnected != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-d.Reconnected:
					if !ok {
						return
					}
					d.Logger.Printf("MQTT reconnected; republishing status")
					if err := publishStatus(payload.EventHeartbeat, d.Now(), ""); err != nil {
						d.Logger.Printf("status after reconnect: %v", err)
					}
				}
			}
		}()
	}

	sweeper := &sweep.Sweeper{Tanks: cfg.Tanks, Source: d.Source, Now: d.Now, Synced: d.Synced}

	// The ticks. In production the scheduler makes them, one per sweep,
	// each as far after the last as the cadence decided; every Work reports
	// to it (below), and lifecycle's OnSkip tells it of a tick lifecycle
	// dropped, which it resends. A test's deps.Tick replaces it with ticks
	// of its own.
	tick := d.Tick
	reportSweep := func(time.Duration) {} // the scheduler's Report, when there is one
	onSkip := tracker.SkippedTick
	if tick == nil {
		after := d.After
		if after == nil {
			after = time.After
		}
		sctx, stop := context.WithCancel(ctx)
		defer stop() // after lifecycle.Run returns: the last sweep has reported by then
		sched := newScheduler(sctx, d.Now, after, tracker.SetNextDue, tracker.SkippedTick)
		// The immediate sweep starts about now, without a tick: it's the
		// first anchor. (Not run's start: the clock sync wait and startup
		// checks have run since.) d.Now() is evaluated here, not in the
		// goroutine.
		go sched.run(d.Now())
		tick, reportSweep = sched.Tick(), sched.Report
		// A tick lifecycle drops here is resent, not lost, so it isn't a
		// skipped tick; the scheduler counts the ones an overrun misses.
		onSkip = sched.Skipped
	}

	return lifecycle.Run(ctx, lifecycle.Config{
		Immediate: true,
		Interval:  cfg.Sweep.Interval, // unused while Tick is set, which it always is
		Heartbeat: cfg.Heartbeat,
		Work: func(ctx context.Context, started time.Time) error {
			// Every Work reports, even one that fails (a cancelled sweep at
			// shutdown), or the scheduler would wait for it for ever. A
			// sweep that didn't complete leaves the cadence as it was.
			next := cad.Status().Interval
			defer func() { reportSweep(next) }()

			sw, err := sweeper.Sweep(ctx)
			if err != nil {
				return err
			}
			// started is lifecycle's start for this unit, from deps.Now
			// (time.Now: monotonic), so a wall-clock step can't move the
			// cadence; sw.At is the wall-clock timestamp it publishes.
			decision := cad.Observe(sw, started)
			next = decision.Interval
			tracker.RecordSweep(sw)
			tracker.RecordCadence(cad.Status())
			// Verbose: every sweep, one line, from the sweep already in hand.
			// It goes before the fault line, so with verbose on a faulty
			// sweep reads as its values, then what was wrong with them.
			vlog.Print(sw.Line())
			var errs []error
			for _, t := range sw.Tanks {
				if err := d.Pub.Publish(mqttpub.Message{
					Topic: payload.StateTopic(prefix, t.DeviceID), Payload: string(payload.State(sw, t)), Retained: true,
				}); err != nil {
					errs = append(errs, fmt.Errorf("publish %s: %w", t.DeviceID, err))
				}
			}
			if faults := sw.Faults(); faults != "" {
				ok, failed := sw.Counts()
				d.Logger.Printf("sweep: %d ok, %d failed, %s: %s", ok, failed, sw.Took(), faults)
			}
			// Verbose: a trigger or a change of mode, after the sweep's own
			// lines, since it's what the sweep decided about the next one.
			if line := decision.Line(); line != "" {
				vlog.Print(line)
			}
			return errors.Join(errs...)
		},
		Ready:         func() bool { return tracker.Snapshot().Ready },
		Emit:          func(e lifecycle.Event) error { return publishStatus(eventName(e.Kind), e.At, e.Reason) },
		OnSkip:        onSkip,
		Now:           d.Now,
		Tick:          tick,
		HeartbeatTick: d.HeartbeatTick,
		Signals:       d.Signals,
		Logger:        d.Logger,
	})
}
