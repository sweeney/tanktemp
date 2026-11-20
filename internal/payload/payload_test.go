package payload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
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
		{Position: "bottom", Order: 1, ROM: "28-000011721ce9", TempC: f(12.0625), RawC: f(12.0625), Code: reading.OK, Attempts: 1, Bus: "w1_bus_master1"},
		{Position: "top", Order: 2, HeightMM: i(1500), ROM: "28-0000117287e6", TempC: f(61.185), RawC: f(61.375), OffsetC: -0.19, Code: reading.OK, Attempts: 1, Bus: "w1_bus_master2"},
	}}
	golden(t, "state-ok.golden", State(sw, tank))
}

// The diagnostic state, for tanktemp's own status page, is the wire state
// with each sensor's attempts and bus appended: the same bytes otherwise.
func TestStateDiagnostic(t *testing.T) {
	sw := sweep.Sweep{At: time.Date(2026, 11, 2, 21, 6, 26, 0, time.UTC), Duration: 3988 * time.Millisecond, ClockSynced: true}
	tank := sweep.Tank{DeviceID: "hot-water-cylinder", Sensors: []sweep.Sensor{
		{Position: "top", Order: 1, ROM: "28-0000117287e6", TempC: f(61.185), RawC: f(61.375), Code: reading.OK, Attempts: 2, Bus: "w1_bus_master2"},
		{Position: "middle", Order: 2, ROM: "28-000011721ab6", Code: reading.NotPresent, Attempts: 1},
	}}
	wire, diag := string(State(sw, tank)), string(StateDiagnostic(sw, tank))
	if strings.Contains(wire, "attempts") || strings.Contains(wire, `"bus"`) {
		t.Errorf("the wire state carries diagnostics: %s", wire)
	}
	want := strings.NewReplacer(`"suspect":null}`, `"suspect":null,"attempts":2,"bus":"w1_bus_master2"}`).Replace(wire)
	want = strings.Replace(want, `"error":"not_present","suspect":null,"attempts":2,"bus":"w1_bus_master2"}`, `"error":"not_present","suspect":null,"attempts":1,"bus":null}`, 1)
	if diag != want {
		t.Errorf("diagnostic state\n got %s\nwant %s", diag, want)
	}
}

func TestGoldenStateFailures(t *testing.T) {
	sw := sweep.Sweep{At: time.Date(2026, 11, 2, 21, 6, 56, 0, time.UTC), Duration: 6402 * time.Millisecond}
	tank := sweep.Tank{DeviceID: "hot-water-cylinder", Sensors: []sweep.Sensor{
		{Position: "bottom", Order: 1, HeightMM: i(100), ROM: "28-000011721ce9", Code: reading.CRC, Attempts: 3, Bus: "w1_bus_master1"},
		{Position: "middle", Order: 2, HeightMM: i(800), ROM: "28-000011721ab6", OffsetC: 0.05, Code: reading.NotPresent, Attempts: 1},
		{Position: "top", Order: 3, HeightMM: i(1500), ROM: "28-0000117287e6", TempC: f(85), RawC: f(85), Code: reading.OK, Suspect: reading.Suspect85, Attempts: 2, Bus: "w1_bus_master2"},
	}}
	golden(t, "state-failures.golden", State(sw, tank))
}

func TestGoldenStatusStartup(t *testing.T) {
	golden(t, "status-startup.golden", Status(StatusInput{
		Event:   EventStartup,
		At:      time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC),
		Version: "4aba4d5",
		Tanks:   []string{"hot-water-cylinder", "second"},
		Meters:  []string{"hot_water_meter"},
		Problems: []startup.Problem{
			{ROM: "28-0000117287e6", Code: startup.CodeParasitePower, Message: "28-0000117287e6 is in parasite-power mode; wire VDD to 3.3 V"},
			{Code: startup.CodeDiscoverFailed, Message: "no 1-Wire bus masters"},
			{Code: startup.CodeGPIORequestFailed, Message: "meter hot_water_meter: GPIO 27 on gpiochip0: permission denied"},
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
		Meters:  []string{"hot_water_meter"},
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
	if got := StateTopic("water/hot/tanktemp", "hot-water-cylinder"); got != "water/hot/tanktemp/hot-water-cylinder/state" {
		t.Errorf("StateTopic = %s", got)
	}
	if got := StatusTopic("test/water/hot/tanktemp"); got != "test/water/hot/tanktemp/status" {
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

func ptr[T any](v T) *T { return &v }

// The meter message, one golden per trigger (docs/WIRE.md, Meter).
func TestGoldenMeterPulse(t *testing.T) {
	golden(t, "meter-pulse.golden", Meter(MeterInput{
		DeviceID: "hot_water_meter", Trigger: TriggerPulse,
		At: time.Date(2026, 11, 20, 7, 5, 4, 7e6, time.UTC), ClockSynced: true, Seq: 1,
		LitresPerPulse: 1, Level: meter.Closed,
		CountingSince: time.Date(2026, 11, 20, 7, 0, 41, 300e6, time.UTC),
		Count:         1,
		LastPulseAt:   ptr(time.Date(2026, 11, 20, 7, 5, 4, 7_400_000, time.UTC)),
	}))
}

func TestGoldenMeterOpen(t *testing.T) {
	golden(t, "meter-open.golden", Meter(MeterInput{
		DeviceID: "hot_water_meter", Trigger: TriggerOpen,
		At: time.Date(2026, 11, 20, 7, 12, 58, 853e6, time.UTC), ClockSynced: true, Seq: 3671,
		LitresPerPulse: 1, Level: meter.Open,
		CountingSince: time.Date(2026, 11, 19, 22, 41, 7, 0, time.UTC),
		Count:         1833,
		LastPulseAt:   ptr(time.Date(2026, 11, 20, 7, 12, 57, 312_900_000, time.UTC)),
		LastInterval:  ptr(6699*time.Millisecond + 800*time.Microsecond),
		LastClosed:    ptr(1541 * time.Millisecond),
		Noise:         15, Short: 1, Glitches: 2, Lost: 3,
	}))
}

// Before the first litre: every "last" is null, the count 0. K = 10 is
// another meter's litres per pulse; the clock not yet synced.
func TestGoldenMeterSweepNone(t *testing.T) {
	golden(t, "meter-sweep-none.golden", Meter(MeterInput{
		DeviceID: "hot_water_meter", Trigger: TriggerSweep,
		At: time.Date(2026, 11, 20, 1, 30, 0, 0, time.UTC), Seq: 7,
		LitresPerPulse: 10, Level: meter.Closed,
		CountingSince: time.Date(2026, 11, 20, 1, 27, 0, 0, time.UTC),
	}))
}

func TestMeterTopic(t *testing.T) {
	if got := MeterTopic("water/hot/tanktemp"); got != "water/hot/tanktemp/meter" {
		t.Errorf("MeterTopic = %s", got)
	}
}

// Some meters have a fractional K (0.1 or 2.5 litres per pulse): it must
// survive the encoding exactly.
func TestMeterFractionalK(t *testing.T) {
	for _, tt := range []struct {
		k    float64
		want string
	}{{0.1, `"litres_per_pulse":0.1,`}, {2.5, `"litres_per_pulse":2.5,`}, {1, `"litres_per_pulse":1,`}} {
		got := string(Meter(MeterInput{DeviceID: "m", Trigger: TriggerSweep, LitresPerPulse: tt.k, Level: meter.Open}))
		if !strings.Contains(got, tt.want) {
			t.Errorf("K %v: %s, want %s", tt.k, got, tt.want)
		}
	}
}

// Trigger and Level are typed: a misspelt value doesn't compile, and the
// encoder spells the real ones as WIRE.md does.
func TestMeterEnumsOnTheWire(t *testing.T) {
	for _, tt := range []struct {
		tr    Trigger
		l     meter.Level
		trig  string
		level string
	}{{TriggerPulse, meter.Closed, "pulse", "closed"}, {TriggerOpen, meter.Open, "open", "open"}, {TriggerSweep, meter.Open, "sweep", "open"}} {
		got := string(Meter(MeterInput{Trigger: tt.tr, Level: tt.l}))
		if !strings.Contains(got, `"trigger":"`+tt.trig+`"`) || !strings.Contains(got, `"level":"`+tt.level+`"`) {
			t.Errorf("%s", got)
		}
	}
}
