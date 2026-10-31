package payload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/startup"
	"github.com/sweeney/tanktemp/internal/sweep"
)

// The goldens are the wire contract (docs/WIRE.md), written by hand before
// the encoders. A diff is a regression: never edit a golden to make a test
// pass.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s:\n got %s\nwant %s", name, got, want)
	}
	if !json.Valid(got) {
		t.Errorf("%s: not valid JSON", name)
	}
}

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }

func TestGoldenStateOK(t *testing.T) {
	sw := sweep.Sweep{At: time.Date(2026, 11, 2, 21, 6, 26, 0, time.UTC), Duration: 3988 * time.Millisecond, ClockSynced: true}
	tank := sweep.Tank{DeviceID: "hot-water-cylinder", Sensors: []sweep.Sensor{
		{Position: "top", Order: 1, HeightMM: i(1500), ROM: "28-0000117287e6", TempC: f(61.185), RawC: f(61.375), OffsetC: -0.19, Code: reading.OK, Attempts: 1, Bus: "w1_bus_master2"},
		{Position: "bottom", Order: 2, ROM: "28-000011721ce9", TempC: f(12.0625), RawC: f(12.0625), Code: reading.OK, Attempts: 1, Bus: "w1_bus_master1"},
	}}
	golden(t, "state-ok.golden", State(sw, tank))
}

func TestGoldenStateFailures(t *testing.T) {
	sw := sweep.Sweep{At: time.Date(2026, 11, 2, 21, 6, 56, 0, time.UTC), Duration: 6402 * time.Millisecond}
	tank := sweep.Tank{DeviceID: "hot-water-cylinder", Sensors: []sweep.Sensor{
		{Position: "top", Order: 1, HeightMM: i(1500), ROM: "28-0000117287e6", TempC: f(85), RawC: f(85), Code: reading.OK, Suspect: reading.Suspect85, Attempts: 2, Bus: "w1_bus_master2"},
		{Position: "middle", Order: 2, HeightMM: i(800), ROM: "28-000011721ab6", OffsetC: 0.05, Code: reading.NotPresent, Attempts: 1},
		{Position: "bottom", Order: 3, HeightMM: i(100), ROM: "28-000011721ce9", Code: reading.CRC, Attempts: 3, Bus: "w1_bus_master1"},
	}}
	golden(t, "state-failures.golden", State(sw, tank))
}

func TestGoldenStatusStartup(t *testing.T) {
	golden(t, "status-startup.golden", Status(StatusInput{
		Event:   EventStartup,
		At:      time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC),
		Version: "4aba4d5",
		Tanks:   []string{"hot-water-cylinder", "second"},
		Problems: []startup.Problem{
			{ROM: "28-0000117287e6", Code: startup.CodeParasitePower, Message: "28-0000117287e6 is in parasite-power mode; wire VDD to 3.3 V"},
			{Code: startup.CodeDiscoverFailed, Message: "no 1-Wire bus masters"},
		},
	}))
}

func lastSweep(ok, failed int, unexpected ...string) *sweep.Sweep {
	var s []sweep.Sensor
	for n := 0; n < ok; n++ {
		s = append(s, sweep.Sensor{Code: reading.OK})
	}
	for n := 0; n < failed; n++ {
		s = append(s, sweep.Sensor{Code: reading.CRC})
	}
	return &sweep.Sweep{Tanks: []sweep.Tank{{Sensors: s}}, Unexpected: unexpected}
}

func TestGoldenStatusHeartbeat(t *testing.T) {
	golden(t, "status-heartbeat.golden", Status(StatusInput{
		Event:   EventHeartbeat,
		At:      time.Date(2026, 11, 3, 21, 0, 0, 0, time.UTC),
		Version: "4aba4d5",
		Ready:   true,
		Uptime:  86400*time.Second + 700*time.Millisecond,
		Tanks:   []string{"hot-water-cylinder"},
		Last:    lastSweep(9, 1, "28-000011721aa5"),
	}))
}

func TestGoldenStatusShutdown(t *testing.T) {
	golden(t, "status-shutdown.golden", Status(StatusInput{
		Event:   EventShutdown,
		At:      time.Date(2026, 11, 3, 21, 0, 5, 0, time.UTC),
		Version: "4aba4d5",
		Ready:   true,
		Uptime:  86405 * time.Second,
		Tanks:   []string{"hot-water-cylinder"},
		Last:    lastSweep(10, 0),
		Reason:  "SIGTERM",
	}))
}

func TestGoldenLWT(t *testing.T) {
	golden(t, "lwt.golden", LWT())
}

func TestTopics(t *testing.T) {
	if got := StateTopic("energy/hot_water", "hot-water-cylinder"); got != "energy/hot_water/hot-water-cylinder/state" {
		t.Errorf("StateTopic = %s", got)
	}
	if got := StatusTopic("test/energy/hot_water"); got != "test/energy/hot_water/status" {
		t.Errorf("StatusTopic = %s", got)
	}
}

func TestTimestampIsUTCToTheSecond(t *testing.T) {
	bst := time.FixedZone("BST", 3600)
	sw := sweep.Sweep{At: time.Date(2026, 6, 1, 12, 0, 0, 999_000_000, bst)}
	var got struct{ Timestamp string }
	_ = json.Unmarshal(State(sw, sweep.Tank{DeviceID: "x"}), &got)
	if got.Timestamp != "2026-06-01T11:00:00Z" {
		t.Errorf("timestamp = %s", got.Timestamp)
	}
}

func TestEmptyTankHasAnEmptyArray(t *testing.T) {
	var got struct{ Sensors []any }
	b := State(sweep.Sweep{}, sweep.Tank{DeviceID: "x"})
	_ = json.Unmarshal(b, &got)
	if got.Sensors == nil {
		t.Errorf("sensors must be [] not null: %s", b)
	}
}
