package onewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/reading/readingtest"
)

// expectFile is a scenario's expect.json (see testdata/gen_fixtures.py).
type expectFile struct {
	Description string       `json:"description"`
	Synthetic   bool         `json:"synthetic"`
	Attempts    int          `json:"attempts"`
	Request     []string     `json:"request"`
	Steps       []expectStep `json:"steps"`
}

type expectStep struct {
	Topology map[string][]string      `json:"topology"`
	Readings map[string]expectReading `json:"readings"`
	Info     map[string]expectInfo    `json:"info"`
}

type expectReading struct {
	Code     string `json:"code"`
	MilliC   *int   `json:"milli_c"`
	Attempts int    `json:"attempts"`
	Suspect  string `json:"suspect"`
}

type expectInfo struct {
	ExtPower   *bool `json:"ext_power"`
	Resolution *int  `json:"resolution"`
}

const scenarioRoot = "testdata/sysfs"

func loadScenario(t *testing.T, name string) (expectFile, []string) {
	t.Helper()
	dir := filepath.Join(scenarioRoot, name)
	data, err := os.ReadFile(filepath.Join(dir, "expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e expectFile
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	steps, _ := filepath.Glob(filepath.Join(dir, "step-*"))
	sort.Strings(steps)
	if len(steps) == 0 {
		steps = []string{dir}
	}
	if len(steps) != len(e.Steps) {
		t.Fatalf("%s: %d step directories, %d expected steps", name, len(steps), len(e.Steps))
	}
	return e, steps
}

func scenarioNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(scenarioRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) < 10 {
		t.Fatalf("only %d scenarios under %s", len(names), scenarioRoot)
	}
	return names
}

// TestScenarios is the handler's main test: every directory under
// testdata/sysfs is a real capture or a synthetic fault, with the readings,
// topology and info the handler must produce. Adding a fixture adds a case.
func TestScenarios(t *testing.T) {
	for _, name := range scenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			e, steps := loadScenario(t, name)
			for i, dir := range steps {
				want := e.Steps[i]
				src := &Source{FS: os.DirFS(dir), Attempts: e.Attempts}
				ctx := context.Background()

				top, err := src.Discover(ctx)
				if err != nil {
					t.Fatalf("step %d: Discover: %v", i+1, err)
				}
				for _, bus := range top.Buses() {
					got := strings.Join(top.OnBus(bus), ",")
					if exp := strings.Join(want.Topology[bus], ","); got != exp {
						t.Errorf("step %d: bus %s = [%s], want [%s]", i+1, bus, got, exp)
					}
				}
				if len(top.Buses()) != len(want.Topology) {
					t.Errorf("step %d: buses %v, want %d", i+1, top.Buses(), len(want.Topology))
				}

				got := src.Read(ctx, top, e.Request)
				if len(got) != len(e.Request) {
					t.Errorf("step %d: %d readings for %d ROMs", i+1, len(got), len(e.Request))
				}
				for rom, w := range want.Readings {
					r := got[rom]
					if err := r.Check(); err != nil {
						t.Errorf("step %d: %v", i+1, err)
					}
					if string(r.Code) != w.Code || r.Attempts != w.Attempts || r.Suspect != w.Suspect {
						t.Errorf("step %d: %s = %s/%d attempts/suspect %q, want %s/%d/%q",
							i+1, rom, r.Code, r.Attempts, r.Suspect, w.Code, w.Attempts, w.Suspect)
					}
					if (r.MilliC == nil) != (w.MilliC == nil) || (r.MilliC != nil && *r.MilliC != *w.MilliC) {
						t.Errorf("step %d: %s value = %v, want %v", i+1, rom, deref(r.MilliC), deref(w.MilliC))
					}
					if bus, _ := top.Bus(rom); r.Bus != bus {
						t.Errorf("step %d: %s Bus = %q, want %q", i+1, rom, r.Bus, bus)
					}
				}

				for rom, w := range want.Info {
					info, err := src.Inspect(ctx, rom)
					if err != nil {
						t.Errorf("step %d: Inspect %s: %v", i+1, rom, err)
						continue
					}
					if !sameBool(info.ExtPower, w.ExtPower) || !sameInt(info.Resolution, w.Resolution) {
						t.Errorf("step %d: Inspect %s = ext_power %v resolution %v, want %v %v",
							i+1, rom, derefB(info.ExtPower), deref(info.Resolution), derefB(w.ExtPower), deref(w.Resolution))
					}
				}
			}
		})
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefB(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

func sameBool(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func sameInt(a, b *int) bool   { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func TestSourceKeepsTheContract(t *testing.T) {
	readingtest.Contract(t, func(t *testing.T) readingtest.Fixture {
		e, steps := loadScenario(t, "rig-steady")
		var present []string
		for rom, r := range e.Steps[0].Readings {
			if r.Code == "ok" {
				present = append(present, rom)
			}
		}
		sort.Strings(present)
		return readingtest.Fixture{
			Source:  &Source{FS: os.DirFS(steps[0]), Attempts: 3},
			Present: present,
			Absent:  "28-0000deadbeef",
		}
	})
}

// seqFS serves a different w1_slave body on each read, like a real sensor
// whose state changes between conversions. Discovery files come from base.
type seqFS struct {
	mu     sync.Mutex
	base   fstest.MapFS
	bodies []string
	err    error // returned by w1_slave reads once bodies run out, if set
	n      int
	onRead func(n int)
}

func (f *seqFS) Open(name string) (fs.File, error) {
	if !strings.HasSuffix(name, "/w1_slave") {
		return f.base.Open(name)
	}
	f.mu.Lock()
	n := f.n
	f.n++
	f.mu.Unlock()
	if f.onRead != nil {
		f.onRead(n)
	}
	if n >= len(f.bodies) && f.err != nil {
		return nil, f.err
	}
	body := f.bodies[min(n, len(f.bodies)-1)]
	return fstest.MapFS{name: {Data: []byte(body)}}.Open(name)
}

const rom1 = "28-00001171f570"

func oneSensor() fstest.MapFS {
	return fstest.MapFS{"w1_bus_master1/w1_master_slaves": {Data: []byte(rom1 + "\n")}}
}

var (
	por     = slave(0x50, 0x05, 0x4b, 0x46, 0x7f, 0xff, 0x0c, 0x10) // power-on reset 85
	real85  = slave(0x50, 0x05, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10) // a genuine 85.000
	t18812  = slave(0x2d, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x03, 0x10)
	t60000  = slave(0xc0, 0x03, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10)
	crcNo   = "27 01 4b 46 7f ff 09 10 73 : crc=72 NO\n27 01 4b 46 7f ff 09 10 73 t=18437\n"
	busLowB = "00 00 00 00 00 00 00 00 00 : crc=00 YES\n00 00 00 00 00 00 00 00 00 t=0\n"
)

func readOne(t *testing.T, f fs.FS, attempts int) reading.Reading {
	t.Helper()
	src := &Source{FS: f, Attempts: attempts}
	top, err := src.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := src.Read(context.Background(), top, []string{rom1})[rom1]
	if err := r.Check(); err != nil {
		t.Error(err)
	}
	return r
}

func TestRetryRules(t *testing.T) {
	tests := []struct {
		name     string
		bodies   []string
		attempts int
		code     reading.Code
		milliC   int
		tries    int
		suspect  string
	}{
		{"good first time", []string{t18812}, 3, reading.OK, 18812, 1, ""},
		{"power-on reset, then a real reading", []string{por, t18812}, 3, reading.OK, 18812, 2, ""},
		{"crc glitch, then a real reading", []string{crcNo, t18812}, 3, reading.OK, 18812, 2, ""},
		{"bus low on every attempt", []string{busLowB}, 3, reading.BusLow, 0, 3, ""},
		{"persistent power-on reset", []string{por}, 3, reading.PowerOnReset, 0, 3, ""},
		{"85 then a different real value: take the second", []string{real85, t60000}, 3, reading.OK, 60000, 2, ""},
		{"85 twice: publish, flagged", []string{real85, real85}, 3, reading.OK, 85000, 2, reading.Suspect85},
		{"85 then a failure: keep the 85, flagged", []string{real85, crcNo}, 3, reading.OK, 85000, 2, reading.Suspect85},
		{"85 after a glitch: one extra read only", []string{crcNo, real85, real85}, 3, reading.OK, 85000, 3, reading.Suspect85},
		{"85 on the last attempt still gets its extra read", []string{crcNo, crcNo, real85, t60000}, 3, reading.OK, 60000, 4, ""},
		{"attempts below 1 means 1", []string{crcNo, t18812}, 0, reading.CRC, 0, 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := readOne(t, &seqFS{base: oneSensor(), bodies: tt.bodies}, tt.attempts)
			if r.Code != tt.code || r.Attempts != tt.tries || r.Suspect != tt.suspect {
				t.Fatalf("got %s after %d attempts, suspect %q; want %s after %d, suspect %q",
					r.Code, r.Attempts, r.Suspect, tt.code, tt.tries, tt.suspect)
			}
			if tt.code == reading.OK && *r.MilliC != tt.milliC {
				t.Errorf("value %d, want %d", *r.MilliC, tt.milliC)
			}
		})
	}
}

func TestUnrecognisedErrorsAreReadErrorsAndRetried(t *testing.T) {
	eio := errors.New("read w1_slave: input/output error")
	r := readOne(t, &seqFS{base: oneSensor(), bodies: nil, err: eio}, 3)
	if r.Code != reading.ReadError || r.Attempts != 3 {
		t.Errorf("got %s after %d, want read_error after 3", r.Code, r.Attempts)
	}
	r = readOne(t, &seqFS{base: oneSensor(), bodies: []string{t18812}, err: nil}, 3)
	if r.Code != reading.OK {
		t.Errorf("sanity: %s", r.Code)
	}
}

func TestCodeFor(t *testing.T) {
	tests := []struct {
		err  error
		want reading.Code
	}{
		{nil, reading.OK},
		{fmt.Errorf("x: %w", ErrNotPresent), reading.NotPresent},
		{fmt.Errorf("x: %w", ErrBusLow), reading.BusLow},
		{fmt.Errorf("x: %w", ErrCRC), reading.CRC},
		{fmt.Errorf("x: %w", ErrPowerOnReset), reading.PowerOnReset},
		{fmt.Errorf("x: %w", ErrBadScratchpad), reading.BadScratchpad},
		{fmt.Errorf("x: %w", ErrOutOfRange), reading.OutOfRange},
		{fmt.Errorf("x: %w", ErrMalformed), reading.Malformed},
		{errors.New("something nobody has seen"), reading.ReadError},
	}
	for _, tt := range tests {
		if got := codeFor(tt.err); got != tt.want {
			t.Errorf("codeFor(%v) = %s, want %s", tt.err, got, tt.want)
		}
	}
}

func TestCancelBetweenAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &seqFS{base: oneSensor(), bodies: []string{crcNo}, onRead: func(int) { cancel() }}
	src := &Source{FS: f, Attempts: 3}
	top, _ := src.Discover(context.Background())
	r := src.Read(ctx, top, []string{rom1})[rom1]
	if r.Code != reading.Cancelled {
		t.Errorf("got %s after %d, want cancelled once the context is done", r.Code, r.Attempts)
	}
	if f.n != 1 {
		t.Errorf("read %d times, want 1: no retry after a cancel", f.n)
	}
}

// slowFS delays every w1_slave read, like a real 750 ms conversion.
type slowFS struct {
	fs.FS
	delay time.Duration
}

func (f slowFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, "/w1_slave") {
		time.Sleep(f.delay)
	}
	return f.FS.Open(name)
}

func TestBusesAreReadInParallel(t *testing.T) {
	// rig-steady: five sensors on each of two buses.
	_, steps := loadScenario(t, "rig-steady")
	src := &Source{FS: slowFS{FS: os.DirFS(steps[0]), delay: 30 * time.Millisecond}, Attempts: 1}
	top, _ := src.Discover(context.Background())
	start := time.Now()
	got := src.Read(context.Background(), top, top.ROMs())
	took := time.Since(start)
	if len(got) != 10 {
		t.Fatalf("%d readings", len(got))
	}
	// Serial would be 10 × 30 ms; per-bus parallel is 5 × 30 ms.
	if took > 250*time.Millisecond {
		t.Errorf("took %v, want ~150ms with the buses in parallel", took)
	}
	if took < 140*time.Millisecond {
		t.Errorf("took %v: reads on one bus must stay in series", took)
	}
}

func TestDiscoverErrors(t *testing.T) {
	if _, err := (&Source{FS: fstest.MapFS{}}).Discover(context.Background()); !errors.Is(err, ErrNoBuses) {
		t.Errorf("no bus masters: err = %v, want ErrNoBuses", err)
	}
	broken := fstest.MapFS{"w1_bus_master1/other": {Data: []byte("x")}}
	if _, err := (&Source{FS: broken}).Discover(context.Background()); err == nil || errors.Is(err, ErrNoBuses) {
		t.Errorf("unreadable master: err = %v", err)
	}
}

func TestInspectAbsent(t *testing.T) {
	_, err := (&Source{FS: oneSensor()}).Inspect(context.Background(), "28-0000deadbeef")
	if !errors.Is(err, ErrNotPresent) {
		t.Errorf("err = %v, want ErrNotPresent", err)
	}
}

func TestInspectUnparseable(t *testing.T) {
	f := oneSensor()
	f[rom1+"/w1_slave"] = &fstest.MapFile{Data: []byte(t18812)}
	f[rom1+"/ext_power"] = &fstest.MapFile{Data: []byte("maybe\n")}
	f[rom1+"/resolution"] = &fstest.MapFile{Data: []byte("twelve\n")}
	info, err := (&Source{FS: f}).Inspect(context.Background(), rom1)
	if err != nil {
		t.Fatal(err)
	}
	if info.ExtPower != nil || info.Resolution != nil {
		t.Errorf("unparseable values must be unknown, not guessed: %+v", info)
	}
}

func TestNewSource(t *testing.T) {
	s := NewSource("/nonexistent", 3)
	if s.Attempts != 3 || s.FS == nil {
		t.Errorf("NewSource = %+v", s)
	}
}

// logLines collects what a Source logs through Logf. Buses are read in
// parallel, so it takes a lock.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func readLogged(t *testing.T, f fs.FS, attempts int) (reading.Reading, []string) {
	t.Helper()
	var l logLines
	src := &Source{FS: f, Attempts: attempts, Logf: l.logf}
	top, err := src.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := src.Read(context.Background(), top, []string{rom1})[rom1]
	return r, l.lines
}

// Issue #2: with verbose on, a retried read says which attempt failed and
// why. Today that detail is lost: only the final outcome reaches the fault
// line, and a read that recovers on its second attempt leaves no trace.
func TestRetriesAreLogged(t *testing.T) {
	tests := []struct {
		name   string
		bodies []string
		want   []string // substrings, one per line, in order
	}{
		{"good first time: nothing", []string{t18812}, nil},
		{"crc, then good: the failed attempt", []string{crcNo, t18812},
			[]string{"read " + rom1 + " attempt 1/3: crc"}},
		{"bus low every time: each attempt", []string{busLowB},
			[]string{"attempt 1/3: bus_low", "attempt 2/3: bus_low", "attempt 3/3: bus_low"}},
		{"power-on reset, then good", []string{por, t18812},
			[]string{"attempt 1/3: power_on_reset"}},
		// The extra read for 85 °C is a retry too (GUESSES G2), so it's
		// logged with its outcome.
		{"85 then a different value", []string{real85, t60000},
			[]string{"attempt 1/3: 85.000 °C, reading once more to check", "extra read: 60.000 °C, keeping it"}},
		{"85 twice", []string{real85, real85},
			[]string{"attempt 1/3: 85.000 °C, reading once more to check", "extra read: 85.000 °C again, flagged 85c_unverified"}},
		{"85 then a failure", []string{real85, crcNo},
			[]string{"attempt 1/3: 85.000 °C, reading once more to check", "extra read failed (crc: " + rom1 + ": scratchpad CRC mismatch), keeping 85.000 °C flagged 85c_unverified"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := readLogged(t, &seqFS{base: oneSensor(), bodies: tt.bodies}, 3)
			if len(got) != len(tt.want) {
				t.Fatalf("logged %d lines, want %d:\n%s", len(got), len(tt.want), strings.Join(got, "\n"))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("line %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// A missing sensor isn't retried, so there's no retry to log; the fault
// line already names it.
func TestNotPresentIsNotLoggedAsARetry(t *testing.T) {
	missing := fstest.MapFS{"w1_bus_master1/w1_master_slaves": {Data: []byte("\n")}}
	r, got := readLogged(t, missing, 3)
	if r.Code != reading.NotPresent || len(got) != 0 {
		t.Errorf("got %s, logged %q; want not_present and nothing logged", r.Code, got)
	}
}

// Logging must not change what is read: the same reads, the same outcomes.
func TestLoggingDoesNotChangeReadings(t *testing.T) {
	for _, bodies := range [][]string{{t18812}, {crcNo, t18812}, {busLowB}, {real85, real85}, {real85, t60000}} {
		quiet := readOne(t, &seqFS{base: oneSensor(), bodies: bodies}, 3)
		f := &seqFS{base: oneSensor(), bodies: bodies}
		logged, _ := readLogged(t, f, 3)
		if quiet.Code != logged.Code || quiet.Attempts != logged.Attempts || quiet.Suspect != logged.Suspect ||
			(quiet.MilliC == nil) != (logged.MilliC == nil) || (quiet.MilliC != nil && *quiet.MilliC != *logged.MilliC) {
			t.Errorf("%d bodies: quiet %+v, logged %+v", len(bodies), quiet, logged)
		}
		if f.n != logged.Attempts {
			t.Errorf("%d bodies: %d sysfs reads for %d attempts: logging must not re-read", len(bodies), f.n, logged.Attempts)
		}
	}
}
