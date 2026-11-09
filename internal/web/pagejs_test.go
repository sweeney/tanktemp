package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sweeney/tanktemp/internal/reading"
)

// The page's polling back-off (review on #4) is the one piece of logic in
// page.html. pollDelay is pure, so these run it under node, cases in,
// results out, with no browser. They test the page as GET / serves it, not
// page.html's source: html/template rewrites the script (it fills in
// {{.IntervalMS}} and strips every JS comment), and the served copy is
// what a browser runs.

// needNode returns node's path. Without node a developer's run skips, but
// in CI (where GitHub sets CI=true) it fails: a skip there would be a
// silent pass (review on #4).
func needNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required in CI to test page.html's script")
		}
		t.Skip("node not installed: page.html's script is untested here")
	}
	return node
}

// servedScript is the page's <script>, as GET / serves it.
func servedScript(t *testing.T) string {
	t.Helper()
	rec, _ := get(t, server(true, sweepAt(reading.OK)), "/")
	page := rec.Body.String()
	b, e := strings.Index(page, "<script>"), strings.Index(page, "</script>")
	if b < 0 || e < b {
		t.Fatal("the served page has no <script>")
	}
	return page[b+len("<script>") : e]
}

// function returns the JS function name's whole declaration from js, by
// matching braces from its first "{". It's for small, plain functions
// like pollDelay: braces in strings or regexps would fool it.
func function(t *testing.T, js, name string) string {
	t.Helper()
	b := strings.Index(js, "function "+name+"(")
	if b < 0 {
		t.Fatalf("no function %s in the served script", name)
	}
	depth := 0
	for i := b + strings.Index(js[b:], "{"); i < len(js); i++ {
		switch js[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[b : i+1]
			}
		}
	}
	t.Fatalf("function %s never closes", name)
	return ""
}

func TestPagePollDelay(t *testing.T) {
	node := needNode(t)
	fn := function(t, servedScript(t), "pollDelay")
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
	script := fn + `
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

// The page's whole script, as served, must parse: a test of one function
// can't see a stray brace elsewhere (one got through while pollDelay was
// extracted).
func TestPageScriptParses(t *testing.T) {
	node := needNode(t)
	f := t.TempDir() + "/page.js"
	if err := os.WriteFile(f, []byte(servedScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Errorf("the served page's script doesn't parse: %v\n%s", err, out)
	}
}
