package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sweeney/common/health/sysinfo"
	"github.com/sweeney/tanktemp/internal/reading"
	"github.com/sweeney/tanktemp/internal/startup"
	"github.com/sweeney/tanktemp/internal/status"
	"github.com/sweeney/tanktemp/internal/sweep"
)

var start = time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)

func server(connected bool, sw *sweep.Sweep, problems ...startup.Problem) *Server {
	tr := status.New(status.Options{Start: start, Version: "abc1234", Tanks: []string{"cyl"}, Interval: 30 * time.Second,
		Connected: func() bool { return connected }})
	tr.SetStartup(startup.Report{Problems: problems})
	if sw != nil {
		tr.RecordSweep(*sw)
	}
	return &Server{
		Tracker: tr,
		Now:     func() time.Time { return start.Add(40 * time.Second) },
		SysFS:   fstest.MapFS{"proc/loadavg": {Data: []byte("0.1 0.2 0.3 1/1 1\n")}},
		Disk:    func() (sysinfo.Disk, error) { return sysinfo.Disk{TotalBytes: 1 << 30, FreeBytes: 1 << 29}, nil },
		Prefix:  "test/energy/hot_water",
	}
}

func sweepAt(code reading.Code) *sweep.Sweep {
	v := 61.185
	r := 61.375
	s := sweep.Sensor{Position: "top", Order: 1, ROM: "28-0000117287e6", Code: code, Attempts: 1, Bus: "w1_bus_master1", OffsetC: -0.19}
	if code == reading.OK {
		s.TempC, s.RawC = &v, &r
	}
	return &sweep.Sweep{At: start.Add(30 * time.Second), Duration: 4 * time.Second, ClockSynced: true,
		Tanks: []sweep.Tank{{DeviceID: "cyl", Sensors: []sweep.Sensor{s}}}, Unexpected: []string{"28-0000000000ee"}}
}

func get(t *testing.T, s *Server, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec, body
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name      string
		connected bool
		sw        *sweep.Sweep
		status    string
		detail    string
	}{
		{"starting", true, nil, "starting", "no complete sweep"},
		{"ok", true, sweepAt(reading.OK), "ok", ""},
		{"degraded by a sensor", true, sweepAt(reading.BusLow), "degraded", "bus_low"},
		{"degraded by mqtt", false, sweepAt(reading.OK), "degraded", "MQTT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := get(t, server(tt.connected, tt.sw), "/healthz")
			if rec.Code != 200 {
				t.Errorf("code %d: always 200", rec.Code)
			}
			if body["status"] != tt.status || body["version"] != "abc1234" {
				t.Errorf("body = %v", body)
			}
			d, _ := body["detail"].(string)
			if !strings.Contains(d, tt.detail) || (tt.detail == "" && d != "") {
				t.Errorf("detail = %q, want %q", d, tt.detail)
			}
			if body["uptime_s"].(float64) != 40 {
				t.Errorf("uptime_s = %v", body["uptime_s"])
			}
			for _, k := range []string{"ready", "mqtt_connected", "tanks", "last_sweep", "sweeps", "skipped_ticks", "unexpected", "startup_problems", "system"} {
				if _, ok := body[k]; !ok {
					t.Errorf("missing %s", k)
				}
			}
		})
	}
}

func TestHealthzKeyOrder(t *testing.T) {
	rec, _ := get(t, server(true, sweepAt(reading.OK)), "/healthz")
	b := rec.Body.String()
	last := -1
	for _, k := range []string{`"status"`, `"version"`, `"uptime_s"`, `"goroutines"`, `"ready"`, `"tanks"`, `"system"`} {
		i := strings.Index(b, k)
		if i < last {
			t.Fatalf("%s out of order in %s", k, b)
		}
		last = i
	}
}

func TestHealthzSummaries(t *testing.T) {
	_, body := get(t, server(true, sweepAt(reading.CRC)), "/healthz")
	tanks := body["tanks"].([]any)
	tk := tanks[0].(map[string]any)
	if tk["device_id"] != "cyl" || tk["sensors_ok"].(float64) != 0 || tk["sensors_failed"].(float64) != 1 {
		t.Errorf("tanks = %v", tanks)
	}
	ls := body["last_sweep"].(map[string]any)
	if ls["timestamp"] != "2026-11-02T21:00:30Z" || ls["age_s"].(float64) != 10 || ls["sweep_ms"].(float64) != 4000 {
		t.Errorf("last_sweep = %v", ls)
	}
	if sys := body["system"].(map[string]any); sys["load_1"].(float64) != 0.1 || sys["disk_total_mb"].(float64) != 1024 {
		t.Errorf("system = %v", sys)
	}
}

func TestHealthzBeforeFirstSweep(t *testing.T) {
	_, body := get(t, server(true, nil), "/healthz")
	if body["last_sweep"] != nil || body["unexpected"] != nil || body["ready"] != false {
		t.Errorf("before the first sweep, unknowns are null: %v", body)
	}
}

func TestAPIStatus(t *testing.T) {
	rec, body := get(t, server(true, sweepAt(reading.OK), startup.Problem{ROM: "28-0000117287e6", Code: "parasite_power", Message: "parasite"}), "/api/status")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("code %d headers %v", rec.Code, rec.Header())
	}
	h := body["health"].(map[string]any)
	if h["status"] != "degraded" {
		t.Errorf("health = %v", h)
	}
	tanks := body["tanks"].([]any)
	tk := tanks[0].(map[string]any)
	sensors := tk["sensors"].([]any)
	s := sensors[0].(map[string]any)
	// The tanks are the wire payload itself, so the page and MQTT never disagree.
	if tk["device_id"] != "cyl" || s["temp_c"].(float64) != 61.185 || s["error"] != nil {
		t.Errorf("tank = %v", tk)
	}
	if body["topic_prefix"] != "test/energy/hot_water" || body["calibrated"] != true || body["interval_s"].(float64) != 30 {
		t.Errorf("body = %v", body)
	}
	if p := body["startup_problems"].([]any); len(p) != 1 {
		t.Errorf("problems = %v", p)
	}
}

func TestAPIStatusUncalibrated(t *testing.T) {
	sw := sweepAt(reading.OK)
	sw.Tanks[0].Sensors[0].OffsetC = 0
	_, body := get(t, server(true, sw), "/api/status")
	if body["calibrated"] != false {
		t.Errorf("calibrated = %v", body["calibrated"])
	}
}

func TestPage(t *testing.T) {
	rec, _ := get(t, server(true, sweepAt(reading.OK)), "/")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("code %d type %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	b := rec.Body.String()
	for _, want := range []string{"<title>tanktemp</title>", "/api/status", "abc1234", "prefers-color-scheme"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	rec, _ = get(t, server(true, nil), "/nope")
	if rec.Code != 404 {
		t.Errorf("/nope = %d", rec.Code)
	}
}
