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
	Log           Log           `yaml:"log"`
	Tanks         []Tank        `yaml:"tanks"`
}

// Sweep is how often and how hard the sensors are read.
type Sweep struct {
	// Interval is the slow interval: the longest gap between two sweeps'
	// starts. With Fast nil it is the only interval, as before issue #1.
	Interval time.Duration `yaml:"interval"`
	Attempts int           `yaml:"attempts"`
	// Fast is the adaptive cadence (issue #1); nil (the default) means a
	// fixed Interval. See Fast and internal/cadence.
	Fast *Fast `yaml:"fast"`
}

// Fast is sweep.fast: sweep faster while any probe is changing quickly
// (issue #1). internal/cadence implements it; this is only what the file
// says.
//
//	sweep:
//	  interval: 30s              # slow
//	  fast:
//	    interval: 10s            # jump straight to this when a probe moves...
//	    trigger_c_per_min: 1.5   # ...at least this fast, either direction
//	    hold: 2m                 # stay at fast.interval this long after the last trigger
//	    decay: 5m                # then ease back to interval over this long
//
// Any key left out of the block takes its DefaultFast value, so "fast: {}"
// turns it on with the defaults. An explicit value is always kept, even a
// zero: "hold: 0s" is a real choice (ease back at once), not a missing key.
// "fast:" with no value at all is refused (checkFastNotNull).
type Fast struct {
	Interval       time.Duration `yaml:"interval"`
	TriggerCPerMin float64       `yaml:"trigger_c_per_min"`
	Hold           time.Duration `yaml:"hold"`
	Decay          time.Duration `yaml:"decay"`
}

// DefaultFast is what a sweep.fast block's missing keys default to. The
// trigger and hold are guesses from two days of recordings (GUESSES G38,
// G39).
var DefaultFast = Fast{
	Interval:       10 * time.Second,
	TriggerCPerMin: 1.5,
	Hold:           2 * time.Minute,
	Decay:          5 * time.Minute,
}

// fastKeys are the keys a sweep.fast block may hold. They must match Fast's
// yaml tags; TestFastKeysMatchTheStruct keeps them in step.
var fastKeys = []string{"interval", "trigger_c_per_min", "hold", "decay"}

// UnmarshalYAML starts from DefaultFast, so a key left out of the block
// keeps its default and an explicit one (zero included) replaces it. A null
// block never gets here: yaml leaves the *Fast nil, which means "off".
//
// It checks the keys itself because yaml.v3's Node.Decode doesn't inherit
// the file decoder's KnownFields, so without this a typo inside the block
// ("intervl") would be ignored and its default used: exactly the silent
// failure KnownFields is there to stop. A YAML merge key ("<<") is refused
// the same way, rather than half-supported.
func (f *Fast) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: sweep.fast must be a mapping of %s", n.Line, strings.Join(fastKeys, ", "))
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		known := false
		for _, want := range fastKeys {
			known = known || k.Value == want
		}
		if !known {
			return fmt.Errorf("line %d: sweep.fast: unknown key %q (known: %s)", k.Line, k.Value, strings.Join(fastKeys, ", "))
		}
	}
	type plain Fast // the same fields without this method, so Decode doesn't recurse
	p := plain(DefaultFast)
	if err := n.Decode(&p); err != nil {
		return err
	}
	*f = Fast(p)
	return nil
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

// Log is how much tanktemp says in the journal (issue #2).
//
// tanktemp has two audiences, so this is one boolean rather than levels:
//
//   - normal running (Verbose false, the default): startup problems, sweeps
//     with faults, MQTT reconnects and the lifecycle, and nothing else;
//   - someone at the bench (Verbose true): all of that, plus one line for
//     every sweep and one for every read that had to be retried.
//
// It is read once at start; changing it means installing the config and
// restarting (./setup-pi.sh), which costs one sweep. There is deliberately
// no SIGHUP or HTTP toggle. If a third audience ever appears, this can grow
// into levels then; until then a level scheme only invites log lines whose
// level nobody decided.
type Log struct {
	// Verbose: log every sweep and every retry. At a 10 s interval that is
	// about 8,640 sweep lines a day, so turn it off again after testing.
	Verbose bool `yaml:"verbose"`
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
		Log:           Log{Verbose: false}, // the zero value, spelled out: quiet unless asked
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
	// MinFastInterval is the shortest sweep.fast.interval (issue #1, GUESSES
	// G41). A state message's timestamp is to the second, and consumers
	// treat a repeated timestamp as a replay (WIRE.md, Cadence), so two
	// sweeps must never start in the same second. Sweeps are spaced from
	// their ticks, and a sweep can start a few ms after its tick, so 1 s
	// isn't quite enough; 2 s leaves a whole second of margin. With the
	// G6 rule, any rig of 3 or more probes is held to more than this anyway.
	MinFastInterval = 2 * time.Second
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
	v.checkFastNotNull(&doc)
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
	v.checkFast(c.Sweep, total)
}

// checkFastNotNull refuses "fast:" with no value (YAML null), which yaml
// decodes as an absent block. That's what's left after commenting out every
// key under it, and taken as "absent" it would silently turn the fast
// cadence off (review on #4). Leaving the block out, or writing fast: {},
// says which was meant.
func (v *validator) checkFastNotNull(doc *yaml.Node) {
	if len(doc.Content) == 0 {
		return
	}
	if f := mapValue(mapValue(doc.Content[0], "sweep"), "fast"); f != nil && f.Tag == "!!null" {
		v.add(f.Line, "sweep.fast", "sweep.fast is empty: leave the block out for a fixed interval, or write fast: {} for the defaults")
	}
}

// checkFast checks sweep.fast (issue #1), if there is one. Each rule is
// what keeps internal/cadence's arithmetic meaningful:
//
//   - fast.interval at least the G6 minimum (one clean read of every
//     sensor), as sweep.interval is, and at least MinFastInterval, so
//     timestamps never repeat;
//   - fast.interval shorter than interval: the decay eases from fast up to
//     slow, and a "fast" that isn't faster has nothing to ease between;
//   - trigger_c_per_min a finite number above 0: 0 would trigger on any
//     change at all, and NaN would never compare true;
//   - hold at least 0 (0 means start easing back at once);
//   - decay above 0: it divides the exponent, and a zero decay would be a
//     cliff from fast to slow, which is what hold already is.
func (v *validator) checkFast(s Sweep, total int) {
	f := s.Fast
	if f == nil {
		return
	}
	need := time.Duration(total) * ReadTime
	switch {
	case f.Interval < MinFastInterval:
		v.add(0, "sweep.fast.interval", "%v is under %v: two sweeps' timestamps (to the second) could repeat", f.Interval, MinFastInterval)
	case total > 0 && f.Interval < need:
		v.add(0, "sweep.fast.interval", "%v is shorter than one clean read of all %d sensors (%v)", f.Interval, total, need)
	}
	if f.Interval >= s.Interval {
		v.add(0, "sweep.fast.interval", "%v must be shorter than sweep.interval (%v)", f.Interval, s.Interval)
	}
	if !(f.TriggerCPerMin > 0) || math.IsInf(f.TriggerCPerMin, 1) {
		v.add(0, "sweep.fast.trigger_c_per_min", "%v must be greater than 0 (and finite)", f.TriggerCPerMin)
	}
	if f.Hold < 0 {
		v.add(0, "sweep.fast.hold", "%v is negative", f.Hold)
	}
	if f.Decay <= 0 {
		v.add(0, "sweep.fast.decay", "%v must be positive", f.Decay)
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
