package web

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/sweeney/tanktemp/internal/reading"
)

// The status page's meter card (its wording) comes from meterView, pure,
// so it runs under node like pollDelay: a meter as /api/status gives it,
// and now, in; what the card says, out.
func TestPageMeterView(t *testing.T) {
	node := needNode(t)
	js := servedScript(t)
	fns := function(t, js, "ago") + "\n" + function(t, js, "litres") + "\n" + function(t, js, "meterView")
	now := `Date.parse("2026-11-21T09:40:00Z")`
	type view struct {
		State    string   `json:"state"`
		Class    string   `json:"cls"`
		Headline string   `json:"headline"`
		Lines    []string `json:"lines"`
		Warnings []string `json:"warnings"`
	}
	for _, tt := range []struct {
		name, meter string
		want        view
	}{
		{"counting, after a draw", `{"device_id":"hot_water_meter","line":"gpiochip0:27","state":"counting","reason":null,"litres_per_pulse":1,
			"level":"open","count":131,"counting_since":"2026-11-21T09:25:33Z","last_pulse_at":"2026-11-21T09:36:17.668Z","last_interval_ms":12449,
			"noise":0,"short":0,"glitches":0,"lost":0,"publish_dropped":0,"queued":0}`,
			view{"counting", "ok", "131 L", []string{
				"last litre 3 min ago, 12.4 s after the one before (4.8 L/min)",
				"reed open · gpiochip0:27",
			}, nil}},
		{"no litre yet, resting closed", `{"device_id":"m","line":"gpiochip0:27","state":"counting","reason":null,"litres_per_pulse":1,
			"level":"closed","count":0,"counting_since":"2026-11-21T09:25:33Z","last_pulse_at":null,"last_interval_ms":null,
			"noise":0,"short":0,"glitches":0,"lost":0,"publish_dropped":0,"queued":0}`,
			view{"counting", "ok", "0 L", []string{"no litre yet", "reed closed · gpiochip0:27"}, nil}},
		{"one litre: no interval yet; K 0.5", `{"device_id":"m","line":"gpiochip0:27","state":"counting","reason":null,"litres_per_pulse":0.5,
			"level":"closed","count":3,"counting_since":"2026-11-21T09:25:33Z","last_pulse_at":"2026-11-21T09:39:55Z","last_interval_ms":null,
			"noise":0,"short":0,"glitches":0,"lost":0,"publish_dropped":0,"queued":0}`,
			view{"counting", "ok", "1.5 L", []string{"last litre 5 s ago", "reed closed · gpiochip0:27"}, nil}},
		{"counters and drops are warnings", `{"device_id":"m","line":"gpiochip0:27","state":"counting","reason":null,"litres_per_pulse":1,
			"level":"open","count":9,"counting_since":"2026-11-21T09:25:33Z","last_pulse_at":"2026-11-19T09:40:00Z","last_interval_ms":600000,
			"noise":15,"short":1,"glitches":0,"lost":3,"publish_dropped":2,"queued":0}`,
			view{"counting", "ok", "9 L", []string{"last litre 2 d ago, 10 min after the one before (0.1 L/min)", "reed open · gpiochip0:27"},
				[]string{"noise 15", "short 1", "lost 3: the count is a lower bound", "2 messages dropped by a slow broker"}}},
		{"failed", `{"device_id":"m","line":"gpiochip0:27","state":"failed","reason":"GPIO 27 on gpiochip0: device or resource busy","litres_per_pulse":1,
			"level":null,"count":null,"counting_since":null,"last_pulse_at":null,"last_interval_ms":null,
			"noise":null,"short":null,"glitches":null,"lost":null,"publish_dropped":0,"queued":0}`,
			view{"failed", "degraded", "not counting", []string{"gpiochip0:27"}, []string{"GPIO 27 on gpiochip0: device or resource busy"}}},
		{"starting", `{"device_id":"m","line":"gpiochip0:27","state":"starting","reason":null,"litres_per_pulse":1,
			"level":null,"count":null,"counting_since":null,"last_pulse_at":null,"last_interval_ms":null,
			"noise":null,"short":null,"glitches":null,"lost":null,"publish_dropped":0,"queued":0}`,
			view{"starting", "starting", "starting", []string{"gpiochip0:27"}, nil}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, err := exec.Command(node, "-e", fns+"\nconsole.log(JSON.stringify(meterView("+tt.meter+", "+now+")));").CombinedOutput()
			if err != nil {
				t.Fatalf("node: %v\n%s", err, out)
			}
			var got view
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("node said %s (%v)", out, err)
			}
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(tt.want)
			if string(g) != string(w) {
				t.Errorf("meterView =\n%s\nwant\n%s", g, w)
			}
		})
	}
}

// The page has somewhere to put the meters.
func TestPageHasAMetersSection(t *testing.T) {
	rec, _ := get(t, server(true, sweepAt(reading.OK)), "/")
	if !strings.Contains(rec.Body.String(), `id="meters"`) {
		t.Error("no meters section")
	}
}
