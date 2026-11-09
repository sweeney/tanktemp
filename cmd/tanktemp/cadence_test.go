package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sweeney/tanktemp/internal/config"
)

// fastConfig is testConfig with the issue's sweep.fast block.
func fastConfig(t *testing.T) config.Config {
	t.Helper()
	c := testConfig(t)
	f := config.DefaultFast // 10s, 1.5 °C/min, hold 2m, decay 5m
	c.Sweep.Fast = &f
	return c
}

// run hands the cadence to /api/status: slow and adaptive before any draw.
func TestAPIStatusReportsTheCadence(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := startRigHTTP(t, cleanSource(), fastConfig(t), ln)
	r.waitFor(t, 3)
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Sweep struct {
			Adaptive  bool    `json:"adaptive"`
			Mode      string  `json:"mode"`
			IntervalS float64 `json:"interval_s"`
		} `json:"sweep"`
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if err != nil || !body.Sweep.Adaptive || body.Sweep.Mode != "slow" || body.Sweep.IntervalS != 30 {
		t.Errorf("/api/status sweep = %+v (err %v)", body.Sweep, err)
	}
	r.sig <- syscall.SIGTERM
	<-r.done
}

// -check-config says what the cadence is, before "verbose log" and
// "publishing to" (deploy/lib.sh reads those from the end of the line).
func TestCheckConfigShowsTheCadence(t *testing.T) {
	dir := t.TempDir()
	base := "mqtt: {broker: tcp://b:1883}\ntanks:\n  - device_id: t\n    sensors:\n      - {rom: 28-0000117287e6, position: top, order: 1}\n"
	for _, tt := range []struct{ extra, want string }{
		{"", ", sweep every 30s, verbose log off, publishing to "},
		{"sweep: {interval: 1m}\n", ", sweep every 1m, verbose log off, "},
		{"sweep: {fast: {}}\n", ", sweep every 30s, 10s while a probe changes 1.5 °C/min (hold 2m, decay 5m), verbose log off, "},
		{"sweep: {fast: {interval: 5s, trigger_c_per_min: 2, hold: 0s, decay: 90s}}\n", ", sweep every 30s, 5s while a probe changes 2 °C/min (hold 0s, decay 1m30s), "},
		{"sweep: {interval: 2h, fast: {hold: 1h, decay: 3h0m}}\n", ", sweep every 2h, 10s while a probe changes 1.5 °C/min (hold 1h, decay 3h), "},
	} {
		path := filepath.Join(dir, "c.yaml")
		_ = os.WriteFile(path, []byte(base+tt.extra), 0o644)
		var out strings.Builder
		if code := realMain([]string{"-config", path, "-check-config"}, &out); code != 0 || !strings.Contains(out.String(), tt.want) {
			t.Errorf("%q: code %d, out %q\nwant it to contain %q", tt.extra, code, out.String(), tt.want)
		}
	}
}
