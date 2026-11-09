// Package web serves tanktemp's status: /healthz (common/health, always 200),
// /api/status (the full last sweep as JSON, the tanks in exactly their MQTT
// wire shape), and / (a small page that polls /api/status). Everything comes
// from one status.Snapshot, so the three can never disagree.
package web

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/sweeney/common/health"
	"github.com/sweeney/common/health/sysinfo"
	"github.com/sweeney/tanktemp/internal/cadence"
	"github.com/sweeney/tanktemp/internal/payload"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/status"
)

// Server is the HTTP side.
type Server struct {
	Tracker *status.Tracker
	Now     func() time.Time
	SysFS   fs.FS                        // rooted at "/"
	Disk    func() (sysinfo.Disk, error) // root filesystem size
	Prefix  string                       // MQTT topic prefix, shown on the page
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health.Handler(s.healthReport))
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /{$}", s.page)
	return mux
}

type tankSummary struct {
	DeviceID      string `json:"device_id"`
	SensorsOK     int    `json:"sensors_ok"`
	SensorsFailed int    `json:"sensors_failed"`
}

type lastSweep struct {
	Timestamp   string `json:"timestamp"`
	AgeS        int64  `json:"age_s"`
	SweepMS     int64  `json:"sweep_ms"`
	ClockSynced bool   `json:"clock_synced"`
}

type problem struct {
	ROM     *string `json:"rom"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
}

func problems(snap status.Snapshot) []problem {
	out := []problem{}
	for _, p := range snap.Problems {
		var rom *string
		if p.ROM != "" {
			r := p.ROM
			rom = &r
		}
		out = append(out, problem{ROM: rom, Code: p.Code, Message: p.Message})
	}
	return out
}

func (s *Server) healthReport() health.Report {
	now := s.Now()
	snap := s.Tracker.Snapshot()
	h := snap.Health(now)

	var tanks []tankSummary
	var last *lastSweep
	var unexpected []string
	if snap.Last != nil {
		for _, t := range snap.Last.Tanks {
			ts := tankSummary{DeviceID: t.DeviceID}
			for _, x := range t.Sensors {
				if x.Code == reading.OK {
					ts.SensorsOK++
				} else {
					ts.SensorsFailed++
				}
			}
			tanks = append(tanks, ts)
		}
		last = &lastSweep{
			Timestamp:   snap.Last.At.UTC().Format(time.RFC3339),
			AgeS:        int64(now.Sub(snap.Last.At) / time.Second),
			SweepMS:     snap.Last.Duration.Milliseconds(),
			ClockSynced: snap.Last.ClockSynced,
		}
		unexpected = append([]string{}, snap.Last.Unexpected...)
	} else {
		for _, id := range snap.Tanks {
			tanks = append(tanks, tankSummary{DeviceID: id})
		}
	}
	return health.Report{
		Status:  h.Status,
		Detail:  strings.Join(h.Detail, "; "),
		Version: snap.Version,
		Start:   snap.Start,
		Now:     now,
		Fields: []health.Field{
			{Key: "ready", Value: snap.Ready},
			{Key: "mqtt_connected", Value: snap.MQTTConnected},
			{Key: "tanks", Value: tanks},
			{Key: "last_sweep", Value: last},
			{Key: "sweeps", Value: snap.Sweeps},
			{Key: "skipped_ticks", Value: snap.SkippedTicks},
			{Key: "unexpected", Value: unexpected},
			{Key: "startup_problems", Value: problems(snap)},
			{Key: "system", Value: sysinfo.Read(s.SysFS, s.Disk)},
		},
	}
}

type apiStatus struct {
	Health struct {
		Status string   `json:"status"`
		Detail []string `json:"detail"`
	} `json:"health"`
	Version         string            `json:"version"`
	StartTime       string            `json:"start_time"`
	IntervalS       float64           `json:"interval_s"`
	TopicPrefix     string            `json:"topic_prefix"`
	MQTTConnected   bool              `json:"mqtt_connected"`
	Verbose         bool              `json:"verbose"` // log.verbose: true means every sweep is being logged (issue #2)
	Calibrated      bool              `json:"calibrated"`
	Tanks           []json.RawMessage `json:"tanks"`
	Unexpected      []string          `json:"unexpected"`
	DiscoverError   *string           `json:"discover_error"`
	StartupProblems []problem         `json:"startup_problems"`
	Sweep           sweepCadence      `json:"sweep"`
}

// sweepCadence is /api/status's sweep block (issue #1): the adaptive
// cadence, for tuning it. It's computed per request from the tracker and is
// never on the wire: state messages are retained, and a volatile "next
// sweep" would be wrong for anyone reading the retained copy later
// (docs/WIRE.md, Cadence).
//
//	"sweep": {"adaptive": true, "mode": "decay", "interval_s": 14.207,
//	          "next_due": "2026-11-09T08:15:42Z",
//	          "last_trigger": {"at": "…", "probe": "cyl.bottom", "rom": "28-…", "c_per_min": -3.2},
//	          "fast_entries": 17}
//
// Times are absolute, so each stays true for as long as it's displayed,
// and the page counts down from next_due itself. It reports the interval
// itself, not a "decay %", because an interval means the same whatever the
// curve's shape.
type sweepCadence struct {
	Adaptive    bool         `json:"adaptive"`     // sweep.fast is configured
	Mode        string       `json:"mode"`         // slow | hold | decay
	IntervalS   float64      `json:"interval_s"`   // the gap from the last sweep to the next
	NextDue     *string      `json:"next_due"`     // null until the scheduler has said
	LastTrigger *lastTrigger `json:"last_trigger"` // null until the first trigger
	FastEntries int          `json:"fast_entries"` // times it went fast since start
}

type lastTrigger struct {
	At      string  `json:"at"`
	Probe   string  `json:"probe"` // "<device_id>.<position>", the statehouse primary
	ROM     string  `json:"rom"`
	CPerMin float64 `json:"c_per_min"` // signed, to 0.01: negative is falling
}

func cadenceBlock(c cadence.Status, nextDue time.Time) sweepCadence {
	out := sweepCadence{
		Adaptive: c.Enabled, Mode: string(c.Mode), IntervalS: c.Interval.Seconds(), FastEntries: c.FastEntries,
	}
	if !nextDue.IsZero() {
		d := nextDue.UTC().Format(time.RFC3339)
		out.NextDue = &d
	}
	if t := c.LastTrigger; t != nil {
		out.LastTrigger = &lastTrigger{
			At: t.At.UTC().Format(time.RFC3339), Probe: t.Probe(), ROM: t.ROM,
			CPerMin: math.Round(t.CPerMin*100) / 100,
		}
	}
	return out
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	now := s.Now()
	snap := s.Tracker.Snapshot()
	h := snap.Health(now)
	var out apiStatus
	out.Health.Status, out.Health.Detail = h.Status, append([]string{}, h.Detail...)
	out.Version = snap.Version
	out.StartTime = snap.Start.UTC().Format(time.RFC3339)
	out.IntervalS = snap.Interval.Seconds()
	out.TopicPrefix = s.Prefix
	out.MQTTConnected = snap.MQTTConnected
	out.Verbose = snap.Verbose
	out.Tanks = []json.RawMessage{}
	out.StartupProblems = problems(snap)
	out.Sweep = cadenceBlock(snap.Cadence, snap.NextDue)
	if snap.Last != nil {
		for _, t := range snap.Last.Tanks {
			out.Tanks = append(out.Tanks, payload.StateDiagnostic(*snap.Last, t))
			for _, x := range t.Sensors {
				if x.OffsetC != 0 {
					out.Calibrated = true
				}
			}
		}
		out.Unexpected = append([]string{}, snap.Last.Unexpected...)
		if snap.Last.DiscoverError != "" {
			e := snap.Last.DiscoverError
			out.DiscoverError = &e
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	snap := s.Tracker.Snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTmpl.Execute(w, map[string]any{
		"Version":    snap.Version,
		"IntervalMS": snap.Interval.Milliseconds(),
	})
}
