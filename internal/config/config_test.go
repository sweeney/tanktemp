package config

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// valid is the smallest complete config. Every vector below changes one
// thing about it.
const valid = `
mqtt:
  broker: tcp://broker.example:1883
tanks:
  - device_id: hot-water-cylinder
    sensors:
      - rom: 28-0000117287e6
        position: top
        order: 1
        height_mm: 1500
        offset_c: -0.16
      - rom: 28-000011721ce9
        position: bottom
        order: 2
        height_mm: 100
`

func TestDefaults(t *testing.T) {
	c, err := Load([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.W1Root != "/sys/bus/w1/devices" || c.Sweep.Interval != 30*time.Second || c.Sweep.Attempts != 3 ||
		c.ClockSyncWait != 2*time.Minute || c.Heartbeat != 15*time.Minute || c.HTTP.Listen != ":80" ||
		c.MQTT.TopicPrefix != "energy/hot_water" {
		t.Errorf("defaults = %+v", c)
	}
	s := c.Tanks[0].Sensors[0]
	if s.ROM != "28-0000117287e6" || s.Position != "top" || s.Order != 1 || *s.HeightMM != 1500 || s.OffsetC != -0.16 {
		t.Errorf("sensor = %+v", s)
	}
	if b := c.Tanks[0].Sensors[1]; b.OffsetC != 0 {
		t.Errorf("offset_c defaults to 0, got %v", b.OffsetC)
	}
}

func TestOverrides(t *testing.T) {
	c, err := Load([]byte(valid + `
w1_root: /tmp/w1
sweep:
  interval: 1m
  attempts: 2
clock_sync_wait: 0s
heartbeat: 5m
http:
  listen: ":8080"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.W1Root != "/tmp/w1" || c.Sweep.Interval != time.Minute || c.Sweep.Attempts != 2 ||
		c.ClockSyncWait != 0 || c.Heartbeat != 5*time.Minute || c.HTTP.Listen != ":8080" {
		t.Errorf("overrides = %+v", c)
	}
}

func TestSensorsComeBackInOrder(t *testing.T) {
	// Sensors may be written in any order in the file; order: decides.
	swapped := `
mqtt: {broker: tcp://b:1883}
tanks:
  - device_id: t
    sensors:
      - {rom: 28-000011721ce9, position: bottom, order: 2}
      - {rom: 28-0000117287e6, position: top, order: 1}
`
	c, err := Load([]byte(swapped))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tanks[0].Sensors[0].Position != "top" {
		t.Errorf("sensors not sorted by order: %+v", c.Tanks[0].Sensors)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // substring of the error
		line int    // 0: don't check
	}{
		{"not yaml", "tanks: [", "yaml", 0},
		{"unknown key", valid + "colour: blue\n", "colour", 0},
		{"unknown sensor key", strings.Replace(valid, "position: top", "position: top\n        hieght_mm: 3", 1), "hieght_mm", 0},
		{"no tanks", "mqtt: {broker: tcp://b:1883}\n", "no tanks", 0},
		{"no broker", strings.Replace(valid, "  broker: tcp://broker.example:1883\n", "  broker: \"\"\n", 1), "mqtt.broker", 0},
		{"broker not a url", strings.Replace(valid, "tcp://broker.example:1883", "broker.example", 1), "mqtt.broker", 0},
		{"tank without sensors", strings.Replace(valid, "  - device_id: hot-water-cylinder\n", "  - device_id: empty\n    sensors: []\n  - device_id: hot-water-cylinder\n", 1), "no sensors", 0},
		{"bad device id", strings.Replace(valid, "hot-water-cylinder", "Hot Water", 1), "device_id", 5},
		{"device id with a slash", strings.Replace(valid, "hot-water-cylinder", "a/b", 1), "device_id", 5},
		{"bad rom", strings.Replace(valid, "28-0000117287e6", "28-xyz", 1), "rom", 7},
		{"rom of another family", strings.Replace(valid, "28-0000117287e6", "10-0000117287e6", 1), "DS18B20", 7},
		{"uppercase rom", strings.Replace(valid, "28-0000117287e6", "28-0000117287E6", 1), "rom", 7},
		{"duplicate rom", strings.Replace(valid, "28-000011721ce9", "28-0000117287e6", 1), "used twice", 12},
		{"bad position", strings.Replace(valid, "position: top", "position: Top Sensor", 1), "position", 8},
		{"duplicate position", strings.Replace(valid, "position: bottom", "position: top", 1), "position \"top\"", 13},
		{"order from 0", strings.Replace(strings.Replace(valid, "order: 1", "order: 0", 1), "order: 2", "order: 1", 1), "order", 0},
		{"order with a gap", strings.Replace(valid, "order: 2", "order: 3", 1), "order", 0},
		{"duplicate order", strings.Replace(valid, "order: 2", "order: 1", 1), "order", 0},
		{"missing order", strings.Replace(valid, "        order: 2\n", "", 1), "order", 0},
		{"order and height disagree", strings.Replace(strings.Replace(valid, "height_mm: 1500", "height_mm: 10", 1), "height_mm: 100", "height_mm: 900", 1), "height", 0},
		{"equal heights", strings.Replace(valid, "height_mm: 100", "height_mm: 1500", 1), "height", 0},
		{"negative height", strings.Replace(valid, "height_mm: 100", "height_mm: -5", 1), "height_mm", 0},
		{"offset too large", strings.Replace(valid, "offset_c: -0.16", "offset_c: 16", 1), "offset_c", 0},
		{"offset just past ±2", strings.Replace(valid, "offset_c: -0.16", "offset_c: -2.01", 1), "±2 °C", 0},
		{"zero attempts", valid + "sweep: {attempts: 0}\n", "attempts", 0},
		{"too many attempts", valid + "sweep: {attempts: 11}\n", "attempts", 0},
		{"interval too short for the sensors", valid + "sweep: {interval: 1s}\n", "interval", 0},
		{"zero heartbeat", valid + "heartbeat: 0s\n", "heartbeat", 0},
		{"negative clock wait", valid + "clock_sync_wait: -1s\n", "clock_sync_wait", 0},
		{"empty listen", valid + "http: {listen: \"\"}\n", "http.listen", 0},
		{"duplicate device id", valid + `  - device_id: hot-water-cylinder
    sensors:
      - {rom: 28-00001171f570, position: top, order: 1}
`, "device_id \"hot-water-cylinder\" used twice", 16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
			if tt.line > 0 && !strings.Contains(err.Error(), "line "+strconv.Itoa(tt.line)+":") {
				t.Errorf("err = %v, want it to name line %d", err, tt.line)
			}
		})
	}
}

func TestPrefixRules(t *testing.T) {
	base := strings.Replace(valid, "  broker: tcp://broker.example:1883\n", "  broker: tcp://broker.example:1883\n  topic_prefix: %s\n", 1)
	for _, bad := range []string{"a/+/b", "a/#", "/a", "a/", "a//b", `""`} {
		if _, err := Load([]byte(strings.Replace(base, "%s", bad, 1))); err == nil || !strings.Contains(err.Error(), "topic_prefix") {
			t.Errorf("prefix %s: err = %v", bad, err)
		}
	}
	for _, good := range []string{"energy/hot_water", "test/energy/hot_water", "tanks"} {
		c, err := Load([]byte(strings.Replace(base, "%s", good, 1)))
		if err != nil || c.MQTT.TopicPrefix != good {
			t.Errorf("prefix %s: %v", good, err)
		}
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	bad := strings.Replace(strings.Replace(valid, "28-0000117287e6", "28-xyz", 1), "position: bottom", "position: B!", 1)
	_, err := Load([]byte(bad))
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("err = %v, want both problems at once", err)
	}
}

func TestNearMisses(t *testing.T) {
	// Each just inside a rule's edge, so a rule that is too strict fails here.
	for name, y := range map[string]string{
		"device id with digits, _ and -": strings.Replace(valid, "hot-water-cylinder", "cyl_2-a", 1),
		"position with digits and _":     strings.Replace(valid, "position: top", "position: top_2", 1),
		"no heights at all":              strings.NewReplacer("        height_mm: 1500\n", "", "        height_mm: 100\n", "").Replace(valid),
		"only one height":                strings.Replace(valid, "        height_mm: 100\n", "", 1),
		"height zero at the bottom":      strings.Replace(valid, "height_mm: 100", "height_mm: 0", 1),
		"offset at the limit":            strings.Replace(valid, "offset_c: -0.16", "offset_c: -2", 1),
		"one attempt":                    valid + "sweep: {attempts: 1}\n",
		"ten attempts":                   valid + "sweep: {attempts: 10, interval: 1m}\n",
		"interval just long enough":      valid + "sweep: {interval: 1600ms}\n",
		"no clock wait":                  valid + "clock_sync_wait: 0s\n",
	} {
		if _, err := Load([]byte(y)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
