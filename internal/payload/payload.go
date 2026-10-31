// Package payload is tanktemp's MQTT wire contract (docs/WIRE.md): topic
// names and the exact bytes of the state, status and last-will messages.
// Structs, not maps, so the key order is fixed; the golden tests pin it.
package payload

import (
	"encoding/json"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/startup"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// Status events.
const (
	EventStartup   = "STARTUP"
	EventHeartbeat = "HEARTBEAT"
	EventShutdown  = "SHUTDOWN"
	EventLWT       = "LWT"
)

// StateTopic is where a tank's sweeps go.
func StateTopic(prefix, deviceID string) string { return prefix + "/" + deviceID + "/state" }

// StatusTopic is the daemon's lifecycle topic (and last will).
func StatusTopic(prefix string) string { return prefix + "/status" }

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

type state struct {
	DeviceID    string   `json:"device_id"`
	Timestamp   string   `json:"timestamp"`
	ClockSynced bool     `json:"clock_synced"`
	SweepMS     int64    `json:"sweep_ms"`
	Sensors     []sensor `json:"sensors"`
}

type sensor struct {
	Position string   `json:"position"`
	Order    int      `json:"order"`
	HeightMM *int     `json:"height_mm"`
	ROM      string   `json:"rom"`
	TempC    *float64 `json:"temp_c"`
	RawC     *float64 `json:"raw_c"`
	OffsetC  float64  `json:"offset_c"`
	Error    *string  `json:"error"`
	Suspect  *string  `json:"suspect"`
	Attempts int      `json:"attempts"`
	Bus      *string  `json:"bus"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// State is one tank's message for one sweep.
func State(sw sweep.Sweep, tank sweep.Tank) []byte {
	m := state{
		DeviceID:    tank.DeviceID,
		Timestamp:   timestamp(sw.At),
		ClockSynced: sw.ClockSynced,
		SweepMS:     sw.Duration.Milliseconds(),
		Sensors:     []sensor{},
	}
	for _, s := range tank.Sensors {
		var errCode *string
		if s.Code != reading.OK {
			errCode = nullable(string(s.Code))
		}
		m.Sensors = append(m.Sensors, sensor{
			Position: s.Position,
			Order:    s.Order,
			HeightMM: s.HeightMM,
			ROM:      s.ROM,
			TempC:    s.TempC,
			RawC:     s.RawC,
			OffsetC:  s.OffsetC,
			Error:    errCode,
			Suspect:  nullable(s.Suspect),
			Attempts: s.Attempts,
			Bus:      nullable(s.Bus),
		})
	}
	return mustMarshal(m)
}

// StatusInput is everything a status message reports.
type StatusInput struct {
	Event    string
	At       time.Time
	Version  string
	Ready    bool
	Uptime   time.Duration
	Tanks    []string
	Last     *sweep.Sweep // nil before the first complete sweep
	Problems []startup.Problem
	Reason   string // SHUTDOWN only
}

type status struct {
	Online          bool      `json:"online"`
	Event           string    `json:"event"`
	Timestamp       string    `json:"timestamp"`
	Version         string    `json:"version"`
	Ready           bool      `json:"ready"`
	UptimeS         int64     `json:"uptime_s"`
	Tanks           []string  `json:"tanks"`
	SensorsOK       *int      `json:"sensors_ok"`
	SensorsFailed   *int      `json:"sensors_failed"`
	Unexpected      []string  `json:"unexpected"`
	StartupProblems []problem `json:"startup_problems"`
	Reason          string    `json:"reason,omitempty"`
}

type problem struct {
	ROM     *string `json:"rom"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
}

// Status is a lifecycle message.
func Status(in StatusInput) []byte {
	m := status{
		Online:          in.Event != EventShutdown,
		Event:           in.Event,
		Timestamp:       timestamp(in.At),
		Version:         in.Version,
		Ready:           in.Ready,
		UptimeS:         int64(in.Uptime / time.Second),
		Tanks:           append([]string{}, in.Tanks...),
		StartupProblems: []problem{},
	}
	if in.Last != nil {
		ok, failed := in.Last.Counts()
		m.SensorsOK, m.SensorsFailed = &ok, &failed
		m.Unexpected = append([]string{}, in.Last.Unexpected...)
	}
	for _, p := range in.Problems {
		m.StartupProblems = append(m.StartupProblems, problem{ROM: nullable(p.ROM), Code: p.Code, Message: p.Message})
	}
	if in.Event == EventShutdown {
		m.Reason = in.Reason
	}
	return mustMarshal(m)
}

// LWT is the last will: fixed when tanktemp connects, so no timestamp.
func LWT() []byte { return []byte(`{"online":false,"event":"LWT"}`) }

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs of strings, numbers and bools: unreachable
	}
	return b
}
