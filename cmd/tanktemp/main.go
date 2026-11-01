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
		fmt.Fprintf(out, "%s: ok: %d tank%s, %d sensor%s, publishing to %s on %s\n",
			*path, len(cfg.Tanks), plural(len(cfg.Tanks)), n, plural(n), cfg.MQTT.TopicPrefix, cfg.MQTT.Broker)
		return 0
	}
	if err := serve(cfg); err != nil {
		log.Printf("tanktemp: %v", err)
		return 1
	}
	return 0
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

	return run(ctx, cfg, deps{
		Source:      onewire.NewSource(cfg.W1Root, cfg.Sweep.Attempts),
		Pub:         pub,
		Reconnected: pub.Reconnected(),
		Synced:      func() bool { _, err := os.Stat(SyncedFlag); return err == nil },
		Now:         time.Now,
		Listener:    ln,
		SysFS:       os.DirFS("/"),
		Disk:        sysinfo.StatfsDisk("/"),
		Logger:      logger,
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

	Tick          <-chan time.Time // nil: every cfg.Sweep.Interval
	HeartbeatTick <-chan time.Time // nil: every cfg.Heartbeat
	Signals       <-chan os.Signal // nil: SIGINT and SIGTERM

	Listener net.Listener // nil: no HTTP
	SysFS    fs.FS
	Disk     func() (sysinfo.Disk, error)

	Logger  *log.Logger
	Version string
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
	var ids []string
	for _, t := range cfg.Tanks {
		ids = append(ids, t.DeviceID)
	}
	tracker := status.New(status.Options{
		Start: start, Version: d.Version, Tanks: ids,
		Interval: cfg.Sweep.Interval, Connected: d.Pub.Connected,
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
	return lifecycle.Run(ctx, lifecycle.Config{
		Immediate: true,
		Interval:  cfg.Sweep.Interval,
		Heartbeat: cfg.Heartbeat,
		Work: func(ctx context.Context, _ time.Time) error {
			sw, err := sweeper.Sweep(ctx)
			if err != nil {
				return err
			}
			tracker.RecordSweep(sw)
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
				d.Logger.Printf("sweep: %d ok, %d failed, %v: %s", ok, failed, sw.Duration.Truncate(time.Millisecond), faults)
			}
			return errors.Join(errs...)
		},
		Ready:         func() bool { return tracker.Snapshot().Ready },
		Emit:          func(e lifecycle.Event) error { return publishStatus(eventName(e.Kind), e.At, e.Reason) },
		OnSkip:        tracker.SkippedTick,
		Now:           d.Now,
		Tick:          d.Tick,
		HeartbeatTick: d.HeartbeatTick,
		Signals:       d.Signals,
		Logger:        d.Logger,
	})
}
