// Command guessreview serves a local page for answering GUESSES.md, one
// guess at a time, and saves each answer to a JSON file as it's given. It
// listens on loopback only.
//
//	go run ./tools/guessreview            # then open the printed URL
//	go run ./tools/guessreview -open      # and open it in the browser
//
// Answers go to tools/guessreview/answers.json (gitignored), keyed by guess
// id: {"G13": {"choice": 1, "kind": "change", "label": "…", "note": "…",
// "updated_at": "…"}}.
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

//go:embed page.html
var page []byte

const maxBody = 64 << 10

var validID = regexp.MustCompile(`^G[0-9]{1,2}$`)

// store keeps the answers in one JSON file, rewritten whole on each save.
type store struct {
	path string
	mu   sync.Mutex
}

func (s *store) load() (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return m, nil
}

func (s *store) put(id string, answer map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	answer["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	m[id] = raw
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func handler(s *store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/answers", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		m, err := s.load()
		s.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("PUT /api/answers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validID.MatchString(id) {
			http.Error(w, "unknown guess id", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "answer too large", http.StatusRequestEntityTooLarge)
			return
		}
		var answer map[string]any
		if err := json.Unmarshal(body, &answer); err != nil || answer == nil {
			http.Error(w, "an answer is a JSON object", http.StatusBadRequest)
			return
		}
		if err := s.put(id, answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8741", "listen address (keep it on loopback)")
	answers := flag.String("answers", filepath.Join("tools", "guessreview", "answers.json"), "where answers are saved")
	open := flag.Bool("open", false, "open the page in the default browser")
	flag.Parse()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/"
	abs, _ := filepath.Abs(*answers)
	fmt.Printf("Guess review: %s\nAnswers save to %s\nCtrl-C to stop.\n", url, abs)
	if *open {
		cmd := "xdg-open"
		if runtime.GOOS == "darwin" {
			cmd = "open"
		}
		_ = exec.Command(cmd, url).Start()
	}
	log.Fatal(http.Serve(ln, handler(&store{path: *answers})))
}
