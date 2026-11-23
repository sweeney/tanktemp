package payload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/sweeney/common/wire/tanktemp"
)

// Consumers decode tanktemp's messages with common's wire/tanktemp, which
// carries copies of these goldens. Every golden here must decode with it to
// the values it states, so a change to the contract that the shared
// decoder doesn't follow fails here, where the change starts (docs/WIRE.md),
// rather than in a consumer. A new golden needs a case below.
func TestGoldensDecodeWithCommonWire(t *testing.T) {
	cases := map[string]func(t *testing.T, b []byte){
		"state-ok.golden": func(t *testing.T, b []byte) {
			s, err := wire.DecodeState(b)
			if err != nil {
				t.Fatal(err)
			}
			if s.DeviceID != "hot-water-cylinder" || !s.ClockSynced || s.SweepMS != 3988 || len(s.Sensors) != 2 {
				t.Errorf("state %+v", s)
			}
			top := s.Sensors[1]
			if top.Position != "top" || top.HeightMM == nil || *top.HeightMM != 1500 ||
				top.TempC == nil || *top.TempC != 61.185 || top.OffsetC != -0.19 || top.Error != nil {
				t.Errorf("top sensor %+v", top)
			}
			if s.Sensors[0].HeightMM != nil {
				t.Errorf("bottom height_mm %v, want unknown", *s.Sensors[0].HeightMM)
			}
		},
		"state-failures.golden": func(t *testing.T, b []byte) {
			s, err := wire.DecodeState(b)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Sensors) != 3 || s.ClockSynced {
				t.Fatalf("state %+v", s)
			}
			if x := s.Sensors[0]; x.TempC != nil || x.RawC != nil || x.Error == nil || *x.Error != "crc" {
				t.Errorf("failed sensor %+v", x)
			}
			if x := s.Sensors[2]; x.TempC == nil || *x.TempC != 85 || x.Suspect == nil || *x.Suspect != "85c_unverified" {
				t.Errorf("suspect sensor %+v", x)
			}
		},
		"status-startup.golden": func(t *testing.T, b []byte) {
			s := wireStatus(t, b, wire.EventStartup, true)
			if s.Ready || s.SensorsOK != nil || s.Unexpected != nil || len(s.StartupProblems) != 3 ||
				len(s.Meters) != 1 || s.StartupProblems[1].ROM != nil {
				t.Errorf("startup %+v", s)
			}
		},
		"status-heartbeat.golden": func(t *testing.T, b []byte) {
			s := wireStatus(t, b, wire.EventHeartbeat, true)
			if !s.Ready || s.UptimeS != 86400 || s.SensorsOK == nil || *s.SensorsOK != 9 ||
				s.SensorsFailed == nil || *s.SensorsFailed != 1 || len(s.Unexpected) != 1 {
				t.Errorf("heartbeat %+v", s)
			}
		},
		"status-shutdown.golden": func(t *testing.T, b []byte) {
			s := wireStatus(t, b, wire.EventShutdown, false)
			if s.Reason != "SIGTERM" || s.Meters == nil || len(s.Meters) != 0 {
				t.Errorf("shutdown %+v (meters %#v)", s, s.Meters)
			}
		},
		"lwt.golden": func(t *testing.T, b []byte) {
			s := wireStatus(t, b, wire.EventLWT, false)
			if s.Timestamp != nil || s.Version != "" {
				t.Errorf("last will %+v", s)
			}
		},
		"meter-pulse.golden": func(t *testing.T, b []byte) {
			m := wireMeter(t, b, wire.TriggerPulse)
			if m.Seq != 1 || m.Count != 1 || m.LastPulseAt == nil ||
				!m.LastPulseAt.Equal(time.Date(2026, 11, 20, 7, 5, 4, 7e6, time.UTC)) ||
				m.LastIntervalMS != nil || m.LastClosedMS != nil {
				t.Errorf("pulse %+v", m)
			}
		},
		"meter-open.golden": func(t *testing.T, b []byte) {
			m := wireMeter(t, b, wire.TriggerOpen)
			at, ok := m.OpenedAt()
			if !ok || !at.Equal(time.Date(2026, 11, 20, 7, 12, 58, 853e6, time.UTC)) || m.Litres() != 1833 ||
				m.Noise != 15 || m.Short != 1 || m.Glitches != 2 || m.Lost != 3 {
				t.Errorf("open %+v, opened at %v %v", m, at, ok)
			}
		},
		"meter-sweep-none.golden": func(t *testing.T, b []byte) {
			m := wireMeter(t, b, wire.TriggerSweep)
			if m.ClockSynced || m.Count != 0 || m.LitresPerPulse != 10 || m.LastPulseAt != nil {
				t.Errorf("sweep %+v", m)
			}
		},
	}
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		name := filepath.Base(f)
		seen[name] = true
		check, ok := cases[name]
		if !ok {
			t.Errorf("%s has no case here: add one", name)
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(strings.TrimSuffix(name, ".golden"), func(t *testing.T) { check(t, b) })
	}
	for name := range cases {
		if !seen[name] {
			t.Errorf("a case for %s, which isn't a golden", name)
		}
	}
}

func wireStatus(t *testing.T, b []byte, ev wire.Event, online bool) wire.Status {
	t.Helper()
	s, err := wire.DecodeStatus(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Event != ev || s.Online != online {
		t.Fatalf("event %s online %v, want %s %v", s.Event, s.Online, ev, online)
	}
	return s
}

func wireMeter(t *testing.T, b []byte, tr wire.Trigger) wire.Meter {
	t.Helper()
	m, err := wire.DecodeMeter(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Trigger != tr || m.DeviceID != "hot_water_meter" {
		t.Fatalf("trigger %s device %s", m.Trigger, m.DeviceID)
	}
	return m
}
