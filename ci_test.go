package tanktemp_test

// CI's fuzz step is configuration, but it has broken the build without any
// code being wrong (issue #5), so its shape is pinned here like any other
// behaviour. This runs in `go test ./...`, so CI checks itself.
//
// What's pinned, and why:
//
//   - -fuzztime is an execution count ("1000000x"), never a duration. When a
//     time-based -fuzztime runs out while a worker is still busy running or
//     minimising an input, Go's fuzz coordinator reports the run as
//     "context deadline exceeded" and the target FAILs, though the fuzzer
//     found nothing. A loaded shared runner makes that more likely. A count
//     has no deadline to race, and gives the same amount of fuzzing on
//     every run however fast the runner is.
//   - The step has a timeout-minutes. A count has no deadline at all, and
//     go test's -timeout doesn't cover fuzzing: the test binary stops its
//     alarm once the unit tests and seeds have run, before the fuzzing
//     starts (testing.M.Run calls stopAlarm). A run with -timeout 5s and a
//     huge count was seen going on past 2 minutes. Without timeout-minutes
//     a genuinely stuck worker would hold the job until GitHub's 6 h limit.
//   - The step fuzzes exactly the Fuzz targets internal/onewire declares, so
//     a new target can't be left out of CI and a renamed one can't leave
//     the loop fuzzing nothing (go test -fuzz with a pattern that matches
//     no target passes silently).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	ciWorkflow = ".github/workflows/ci.yml"
	fuzzStep   = "fuzz (short)"
	fuzzPkg    = "internal/onewire"
)

// workflow is just enough of a GitHub Actions workflow to find a step.
type workflow struct {
	Jobs map[string]struct {
		Steps []step `yaml:"steps"`
	} `yaml:"jobs"`
}

type step struct {
	Name           string `yaml:"name"`
	Run            string `yaml:"run"`
	TimeoutMinutes int    `yaml:"timeout-minutes"`
}

// ciFuzzStep returns the fuzz step from ci.yml. It must exist exactly once:
// a rename would otherwise let every check below pass vacuously.
func ciFuzzStep(t *testing.T) step {
	t.Helper()
	data, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("%s: %v", ciWorkflow, err)
	}
	var found []step
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if s.Name == fuzzStep {
				found = append(found, s)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want exactly one step named %q, found %d", ciWorkflow, fuzzStep, len(found))
	}
	return found[0]
}

// fuzzTimeArg matches each -fuzztime value in a shell script, with or
// without '=' and quotes.
var fuzzTimeArg = regexp.MustCompile(`-fuzztime[= ]+["']?([^"'\s]+)`)

// execCount is what go test accepts as a count: digits then 'x'. A duration
// ("20s", "1m") or anything else fails.
var execCount = regexp.MustCompile(`^[1-9][0-9]*x$`)

func TestCIFuzzStepRunsAnExecutionCount(t *testing.T) {
	s := ciFuzzStep(t)
	args := fuzzTimeArg.FindAllStringSubmatch(s.Run, -1)
	if len(args) == 0 {
		t.Fatalf("%q has no -fuzztime; without one a fuzz target runs forever", fuzzStep)
	}
	for _, a := range args {
		if !execCount.MatchString(a[1]) {
			t.Errorf("-fuzztime %s: want an execution count like 1000000x, not a duration; "+
				"a time-based run can end in 'context deadline exceeded' with nothing wrong (issue #5)", a[1])
		}
	}
}

func TestCIFuzzStepHasATimeout(t *testing.T) {
	if s := ciFuzzStep(t); s.TimeoutMinutes <= 0 {
		t.Errorf("%q has no timeout-minutes; an execution count has no deadline, "+
			"so a stuck worker would hold the job for GitHub's 6 h default", fuzzStep)
	}
}

// declaredFuzzTargets parses fuzzPkg's test files for func FuzzX(*testing.F).
// It reads the files itself, so go test's cache sees an added target.
func declaredFuzzTargets(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(fuzzPkg, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, f := range files {
		// Build tags are ignored on purpose: a target behind a tag is still
		// one CI should know about, and the hardware tag holds none today.
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Fuzz") && isTestingF(fn) {
				names = append(names, fn.Name.Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// isTestingF reports whether fn takes exactly one *testing.F.
func isTestingF(fn *ast.FuncDecl) bool {
	ps := fn.Type.Params.List
	if len(ps) != 1 || len(ps[0].Names) > 1 {
		return false
	}
	star, ok := ps[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && sel.Sel.Name == "F"
}

// loopTargets matches the step's `for t in A B C; do` list.
var loopTargets = regexp.MustCompile(`for\s+\w+\s+in\s+([^;\n]+?)\s*;\s*do`)

func TestCIFuzzStepCoversEveryTarget(t *testing.T) {
	m := loopTargets.FindStringSubmatch(ciFuzzStep(t).Run)
	if m == nil {
		t.Fatalf("%q: no `for t in …; do` loop over the targets", fuzzStep)
	}
	inCI := strings.Fields(m[1])
	slices.Sort(inCI)
	declared := declaredFuzzTargets(t)
	if len(declared) == 0 {
		t.Fatalf("no Fuzz targets found in %s; has the package moved?", fuzzPkg)
	}
	if !slices.Equal(inCI, declared) {
		t.Errorf("%q fuzzes %v, but %s declares %v", fuzzStep, inCI, fuzzPkg, declared)
	}
}
