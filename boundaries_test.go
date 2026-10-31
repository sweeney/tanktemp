package tanktemp_test

// The core packages hold tanktemp's business logic and must be testable with
// every external component faked: no filesystem, no sysfs, no network, no
// broker. This test turns that intention into a build failure.
//
// Two checks, because they answer different questions:
//   - direct imports: the core must not reach the outside world itself
//     (os, io/fs, net, syscall, …). Checked directly, not transitively,
//     because fmt and time import os and syscall themselves.
//   - transitive deps: the core must not depend, by any path, on the code
//     that does reach it (the 1-Wire handler, the web layer, MQTT).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/sweeney/tanktemp"

// core lists the packages that must stay pure. A package listed here that
// doesn't exist fails the test, so the list can't silently go stale.
var core = []string{
	"internal/config",
	"internal/reading",
	"internal/reading/readingtest",
}

var forbiddenDirect = []string{
	"io/fs", "io/ioutil", "net", "net/http", "os", "os/exec", "os/signal",
	"path/filepath", "syscall",
}

var forbiddenDeps = []string{
	module + "/internal/onewire",
	module + "/internal/web",
	"github.com/eclipse/paho.mqtt.golang",
	"github.com/sweeney/common/mqttpub",
}

func goList(t *testing.T, format, pkg string, deps bool) []string {
	t.Helper()
	args := []string{"list", "-f", format}
	if deps {
		args = append(args, "-deps")
	}
	out, err := exec.Command("go", append(args, pkg)...).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// readSources reads every Go file in dir. go test caches results by the files
// a test opens, and it can't see what `go list` reads, so without this an
// edit that adds a forbidden import could be served a cached pass.
func readSources(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			if _, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCorePackagesAreIsolated(t *testing.T) {
	for _, p := range core {
		pkg := module + "/" + p
		t.Run(p, func(t *testing.T) {
			readSources(t, p)
			for _, imp := range goList(t, `{{join .Imports " "}}`, pkg, false) {
				for _, bad := range forbiddenDirect {
					if imp == bad {
						t.Errorf("imports %s directly; reach the outside world through an interface instead", imp)
					}
				}
			}
			for _, dep := range goList(t, `{{.ImportPath}}`, pkg, true) {
				for _, bad := range forbiddenDeps {
					if dep == bad || strings.HasPrefix(dep, bad+"/") {
						t.Errorf("depends on %s", dep)
					}
				}
			}
		})
	}
}
