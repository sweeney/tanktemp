package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The page's polling back-off (review on #4) is the one piece of logic in
// page.html. pollDelay is pure and marked out in the page, so this runs it
// under node, cases in, results out, with no browser. node is on GitHub's
// ubuntu runners; without it the test says so and skips, rather than
// passing silently.
func TestPagePollDelay(t *testing.T) {
	const begin, end = "// pollDelay: begin", "// pollDelay: end"
	b, e := strings.Index(pageHTML, begin), strings.Index(pageHTML, end)
	if b < 0 || e < b {
		t.Fatalf("page.html has no %q … %q block", begin, end)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: page.html's pollDelay is untested here")
	}
	type tc struct {
		Name      string  `json:"name"`
		NextInS   float64 `json:"next_in_s"`
		Backoff   float64 `json:"backoff"`
		Interval  float64 `json:"interval"`
		WantDelay float64 `json:"want_delay"`
		WantBack  float64 `json:"want_backoff"`
	}
	cases := []tc{
		{"due in 7.5 s: 2 s after it, and the back-off resets", 7.5, 8000, 30000, 9500, 1000},
		{"just due: still 2 s after it", 0.1, 1000, 30000, 2100, 1000},
		{"overdue, first time: 2 s", 0, 1000, 30000, 2000, 2000},
		{"overdue, again: doubles", -3, 2000, 30000, 4000, 4000},
		{"overdue a long time: capped at the interval", -300, 16000, 30000, 30000, 30000},
		{"capped stays capped", -400, 30000, 30000, 30000, 30000},
		{"a short interval caps sooner", -5, 4000, 6000, 6000, 6000},
	}
	in, _ := json.Marshal(cases)
	script := pageHTML[b:e] + `
const cases = ` + string(in) + `;
console.log(JSON.stringify(cases.map(c => pollDelay(c.next_in_s, c.backoff, c.interval))));
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got [][2]float64
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node said %s (%v)", out, err)
	}
	for i, c := range cases {
		if got[i][0] != c.WantDelay || got[i][1] != c.WantBack {
			t.Errorf("%s: pollDelay(%v, %v, %v) = [%v, %v], want [%v, %v]",
				c.Name, c.NextInS, c.Backoff, c.Interval, got[i][0], got[i][1], c.WantDelay, c.WantBack)
		}
	}
}

// The page's whole script must parse: a test of one function can't see a
// stray brace elsewhere (one got through while pollDelay was extracted).
func TestPageScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: page.html's script is unchecked here")
	}
	b, e := strings.Index(pageHTML, "<script>"), strings.Index(pageHTML, "</script>")
	if b < 0 || e < b {
		t.Fatal("page.html has no <script>")
	}
	js := strings.ReplaceAll(pageHTML[b+len("<script>"):e], "{{.IntervalMS}}", "30000")
	f := t.TempDir() + "/page.js"
	if err := os.WriteFile(f, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Errorf("page.html's script doesn't parse: %v\n%s", err, out)
	}
}
