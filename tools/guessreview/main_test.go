package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answers.json")
	s := httptest.NewServer(handler(&store{path: path}))
	t.Cleanup(s.Close)
	return s, path
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestServesThePage(t *testing.T) {
	s, _ := newServer(t)
	code, body := do(t, "GET", s.URL+"/", "")
	if code != 200 || !strings.Contains(body, "<title>tanktemp Guess Review</title>") {
		t.Fatalf("GET / = %d, %.80q", code, body)
	}
	if code, _ := do(t, "GET", s.URL+"/nope", ""); code != 404 {
		t.Errorf("GET /nope = %d, want 404", code)
	}
}

func TestNoAnswersYetIsAnEmptyObject(t *testing.T) {
	s, _ := newServer(t)
	code, body := do(t, "GET", s.URL+"/api/answers", "")
	if code != 200 || strings.TrimSpace(body) != "{}" {
		t.Fatalf("GET answers = %d %q", code, body)
	}
}

func TestPutSavesToTheFileAndSurvivesARestart(t *testing.T) {
	s, path := newServer(t)
	if code, body := do(t, "PUT", s.URL+"/api/answers/G13", `{"choice":1,"kind":"change","note":"see WIRE.md"}`); code != 204 {
		t.Fatalf("PUT = %d %q", code, body)
	}
	if code, _ := do(t, "PUT", s.URL+"/api/answers/G2", `{"choice":0,"kind":"keep","note":""}`); code != 204 {
		t.Fatalf("PUT G2 = %d", code)
	}
	// A second PUT replaces the first.
	do(t, "PUT", s.URL+"/api/answers/G13", `{"choice":2,"kind":"change","note":"walk me through"}`)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("file isn't JSON: %v\n%s", err, raw)
	}
	if onDisk["G13"]["note"] != "walk me through" || onDisk["G2"]["kind"] != "keep" || len(onDisk) != 2 {
		t.Errorf("file = %s", raw)
	}
	if _, ok := onDisk["G13"]["updated_at"]; !ok {
		t.Errorf("no updated_at stamped: %s", raw)
	}

	// A new server on the same file sees them.
	s2 := httptest.NewServer(handler(&store{path: path}))
	defer s2.Close()
	_, body := do(t, "GET", s2.URL+"/api/answers", "")
	var got map[string]map[string]any
	_ = json.Unmarshal([]byte(body), &got)
	if got["G13"]["choice"] != float64(2) {
		t.Errorf("after restart: %s", body)
	}
}

func TestPutRefusesBadInput(t *testing.T) {
	s, path := newServer(t)
	cases := map[string]struct{ id, body string }{
		"unknown id":    {"G99x", `{}`},
		"path-ish id":   {"..", `{}`},
		"not JSON":      {"G1", `nope`},
		"not an object": {"G1", `[1,2]`},
		"too big":       {"G1", `{"note":"` + strings.Repeat("x", 70_000) + `"}`},
	}
	for name, c := range cases {
		if code, _ := do(t, "PUT", s.URL+"/api/answers/"+c.id, c.body); code < 400 || code > 499 {
			t.Errorf("%s: %d, want 4xx", name, code)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused PUT wrote the file")
	}
}
