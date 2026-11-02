// Package config loads and validates tanktemp's local YAML config: which
// sensor (by ROM) sits at which position on which tank. The layout is tied to
// the hardware and has to work at an offline boot, so it lives on the Pi
// (/etc/tanktemp/config.yaml), not in config.swee.net. What a tank *is*
// (capacity, model, room) lives in devices_<site>, keyed by device_id.
//
// Load takes bytes, not a path, so the package never touches the filesystem.
// It refuses anything doubtful and reports every problem at once, with the
// line it is on.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file.
type Config struct {
	W1Root        string        `yaml:"w1_root"`
	Sweep         Sweep         `yaml:"sweep"`
	ClockSyncWait time.Duration `yaml:"clock_sync_wait"`
	Heartbeat     time.Duration `yaml:"heartbeat"`
	HTTP          HTTP          `yaml:"http"`
	MQTT          MQTT          `yaml:"mqtt"`
	Tanks         []Tank        `yaml:"tanks"`
}

// Sweep is how often and how hard the sensors are read.
type Sweep struct {
	Interval time.Duration `yaml:"interval"`
	Attempts int           `yaml:"attempts"`
}

// HTTP is the status server.
type HTTP struct {
	Listen string `yaml:"listen"`
}

// MQTT is the broker connection and topic prefix.
type MQTT struct {
	Broker       string `yaml:"broker"`
	ClientID     string `yaml:"client_id"` // empty: main uses tanktemp-<hostname>
	TopicPrefix  string `yaml:"topic_prefix"`
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"password_file"`
}

// Tank is one cylinder. DeviceID is its key in devices_<site>, its topic
// segment, and the prefix of each probe's identity.
type Tank struct {
	DeviceID string   `yaml:"device_id"`
	Sensors  []Sensor `yaml:"sensors"` // sorted by Order after Load
}

// Sensor is one probe on a tank.
type Sensor struct {
	ROM      string  `yaml:"rom"`
	Position string  `yaml:"position"`
	Order    int     `yaml:"order"`     // explicit: 1 = bottom
	HeightMM *int    `yaml:"height_mm"` // optional
	OffsetC  float64 `yaml:"offset_c"`  // optional calibration, added to the raw reading
}

// Defaults.
const (
	DefaultW1Root      = "/sys/bus/w1/devices"
	DefaultTopicPrefix = "energy/hot_water"
)

func defaults() Config {
	return Config{
		W1Root:        DefaultW1Root,
		Sweep:         Sweep{Interval: 30 * time.Second, Attempts: 3},
		ClockSyncWait: 2 * time.Minute,
		Heartbeat:     15 * time.Minute,
		HTTP:          HTTP{Listen: ":80"},
		MQTT:          MQTT{TopicPrefix: DefaultTopicPrefix},
	}
}

// Limits.
const (
	MaxAttempts = 10
	// MaxOffsetC bounds a calibration offset. DS18B20s are within ±0.5 °C
	// out of the box, so anything beyond ±2 °C is far more likely a typo
	// than a calibration (decided 2026-11-01, GUESSES G7).
	MaxOffsetC = 2.0
	// ReadTime is one DS18B20 conversion at 12-bit, with margin.
	ReadTime = 800 * time.Millisecond
)

var (
	reDeviceID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	rePosition = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
	reROM      = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{12}$`)
)

// ValidationError lists every problem found.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config: " + strings.Join(e.Problems, "; ")
}

// ErrYAML wraps a file that isn't valid YAML for this schema.
var ErrYAML = errors.New("config yaml")

// Load parses and validates a config file's contents.
func Load(data []byte) (Config, error) {
	c := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%w: %v", ErrYAML, err)
	}
	var doc yaml.Node
	_ = yaml.Unmarshal(data, &doc) // already known to parse
	v := validator{lines: lineIndex(&doc)}
	v.check(&c)
	if len(v.problems) > 0 {
		return Config{}, &ValidationError{Problems: v.problems}
	}
	for i := range c.Tanks {
		s := c.Tanks[i].Sensors
		sort.SliceStable(s, func(a, b int) bool { return s[a].Order < s[b].Order })
	}
	return c, nil
}

type validator struct {
	lines    lines
	problems []string
}

func (v *validator) add(line int, path, format string, args ...any) {
	msg := path + ": " + fmt.Sprintf(format, args...)
	if line > 0 {
		msg = fmt.Sprintf("line %d: %s", line, msg)
	}
	v.problems = append(v.problems, msg)
}

func (v *validator) check(c *Config) {
	v.checkService(c)
	if len(c.Tanks) == 0 {
		v.add(0, "tanks", "no tanks configured")
	}
	tankAt := map[string]string{}
	romAt := map[string]string{}
	total := 0
	for ti, tank := range c.Tanks {
		tp := fmt.Sprintf("tanks[%d]", ti)
		line := v.lines.at(ti, -1, "device_id")
		switch {
		case !reDeviceID.MatchString(tank.DeviceID):
			v.add(line, tp+".device_id", "%q must be lowercase letters, digits, _ and - (it is a topic segment)", tank.DeviceID)
		case tankAt[tank.DeviceID] != "":
			v.add(line, tp+".device_id", "device_id %q used twice (also %s)", tank.DeviceID, tankAt[tank.DeviceID])
		default:
			tankAt[tank.DeviceID] = tp
		}
		if len(tank.Sensors) == 0 {
			v.add(v.lines.at(ti, -1, ""), tp, "tank %q has no sensors", tank.DeviceID)
			continue
		}
		total += len(tank.Sensors)
		posAt := map[string]bool{}
		orders := map[int]bool{}
		for si, s := range tank.Sensors {
			sp := fmt.Sprintf("%s.sensors[%d]", tp, si)
			switch {
			case !reROM.MatchString(s.ROM):
				v.add(v.lines.at(ti, si, "rom"), sp+".rom", "%q is not a ROM ID (28-xxxxxxxxxxxx, lowercase hex)", s.ROM)
			case !strings.HasPrefix(s.ROM, "28-"):
				v.add(v.lines.at(ti, si, "rom"), sp+".rom", "%q is not a DS18B20 (family 28); no other family is supported", s.ROM)
			case romAt[s.ROM] != "":
				v.add(v.lines.at(ti, si, "rom"), sp+".rom", "%s used twice (also %s)", s.ROM, romAt[s.ROM])
			default:
				romAt[s.ROM] = sp
			}
			switch {
			case !rePosition.MatchString(s.Position):
				v.add(v.lines.at(ti, si, "position"), sp+".position", "%q must be lowercase letters, digits and _", s.Position)
			case posAt[s.Position]:
				v.add(v.lines.at(ti, si, "position"), sp+".position", "position %q used twice in tank %q", s.Position, tank.DeviceID)
			default:
				posAt[s.Position] = true
			}
			if s.HeightMM != nil && *s.HeightMM < 0 {
				v.add(v.lines.at(ti, si, "height_mm"), sp+".height_mm", "%d is negative", *s.HeightMM)
			}
			if math.Abs(s.OffsetC) > MaxOffsetC || math.IsNaN(s.OffsetC) {
				v.add(v.lines.at(ti, si, "offset_c"), sp+".offset_c", "%v is beyond ±%v °C; is it a typo?", s.OffsetC, MaxOffsetC)
			}
			orders[s.Order] = true
		}
		for o := 1; o <= len(tank.Sensors); o++ {
			if !orders[o] {
				v.add(v.lines.at(ti, -1, "sensors"), tp+".sensors", "order must number the %d sensors 1..%d exactly once (1 = bottom); %d is missing", len(tank.Sensors), len(tank.Sensors), o)
				break
			}
		}
		v.checkHeights(tp, ti, tank)
	}
	if need := time.Duration(total) * ReadTime; total > 0 && c.Sweep.Interval < need {
		v.add(0, "sweep.interval", "%v is shorter than one clean read of all %d sensors (%v)", c.Sweep.Interval, total, need)
	}
}

// checkHeights cross-checks two declared facts: a higher order (further
// up the tank) must have a strictly greater height, wherever both are given.
// Neither is derived from the other.
func (v *validator) checkHeights(tp string, ti int, tank Tank) {
	type oh struct{ order, height, idx int }
	var hs []oh
	for si, s := range tank.Sensors {
		if s.HeightMM != nil {
			hs = append(hs, oh{s.Order, *s.HeightMM, si})
		}
	}
	sort.Slice(hs, func(a, b int) bool { return hs[a].order < hs[b].order })
	for i := 1; i < len(hs); i++ {
		if hs[i].height <= hs[i-1].height {
			v.add(v.lines.at(ti, hs[i].idx, "height_mm"), fmt.Sprintf("%s.sensors[%d].height_mm", tp, hs[i].idx),
				"order %d is at %d mm, not above order %d at %d mm (1 = bottom)", hs[i].order, hs[i].height, hs[i-1].order, hs[i-1].height)
		}
	}
}

func (v *validator) checkService(c *Config) {
	if c.Sweep.Attempts < 1 || c.Sweep.Attempts > MaxAttempts {
		v.add(0, "sweep.attempts", "%d must be 1..%d", c.Sweep.Attempts, MaxAttempts)
	}
	if c.Heartbeat <= 0 {
		v.add(0, "heartbeat", "%v must be positive", c.Heartbeat)
	}
	if c.ClockSyncWait < 0 {
		v.add(0, "clock_sync_wait", "%v is negative", c.ClockSyncWait)
	}
	if c.HTTP.Listen == "" {
		v.add(0, "http.listen", "empty")
	}
	if u, err := url.Parse(c.MQTT.Broker); c.MQTT.Broker == "" || err != nil || u.Host == "" ||
		!map[string]bool{"tcp": true, "ssl": true, "tls": true, "ws": true, "wss": true}[u.Scheme] {
		v.add(0, "mqtt.broker", "%q must be a URL like tcp://host:1883", c.MQTT.Broker)
	}
	p := c.MQTT.TopicPrefix
	if p == "" || strings.ContainsAny(p, "+#") || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		v.add(0, "mqtt.topic_prefix", "%q must be a plain topic path: no wildcards, no empty levels", p)
	}
}

// lines finds source lines for tank and sensor fields in the parsed YAML.
type lines struct{ tanks *yaml.Node }

func lineIndex(doc *yaml.Node) lines {
	if len(doc.Content) == 0 {
		return lines{}
	}
	return lines{tanks: mapValue(doc.Content[0], "tanks")}
}

// at returns the line of tanks[ti] (si < 0) or tanks[ti].sensors[si], at key
// if present, else the item itself; 0 if unknown.
func (l lines) at(ti, si int, key string) int {
	if l.tanks == nil || l.tanks.Kind != yaml.SequenceNode || ti >= len(l.tanks.Content) {
		return 0
	}
	n := l.tanks.Content[ti]
	if si >= 0 {
		s := mapValue(n, "sensors")
		if s == nil || s.Kind != yaml.SequenceNode || si >= len(s.Content) {
			return 0
		}
		n = s.Content[si]
	}
	if v := mapValue(n, key); v != nil {
		return v.Line
	}
	return n.Line
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode || key == "" {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
