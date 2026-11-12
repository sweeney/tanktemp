package tanktemp_test

// CI's fuzz step is configuration, but it broke the build with no code wrong
// (issue #5), so its shape is pinned like any other behaviour, and
// `go test ./...` runs this in CI. These are thin wrappers that run the
// checks in cicheck_test.go on the real repo; the cases are in
// cicheck_table_test.go, and the reasons in GUESSES G52–G53.

import (
	"os"
	"slices"
	"testing"
)

const (
	ciWorkflow = ".github/workflows/ci.yml"
	fuzzStep   = "fuzz (short)"
)

func ciFuzzStep(t *testing.T) step {
	t.Helper()
	data, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	s, err := findStep(data, fuzzStep)
	if err != nil {
		t.Fatalf("%s: %v", ciWorkflow, err)
	}
	return s
}

func TestCIFuzzStepHasATimeout(t *testing.T) {
	if err := checkTimeout(ciFuzzStep(t)); err != nil {
		t.Error(err)
	}
}

// Every fuzz command runs a count, and the step fuzzes exactly the targets
// the module declares, package by package.
func TestCIFuzzStepCoversEveryTarget(t *testing.T) {
	inCI, errs := stepTargets(ciFuzzStep(t).Run)
	for _, err := range errs {
		t.Errorf("%q: %v", fuzzStep, err)
	}
	// os.DirFS goes through os.Open, so go test's cache sees every file and
	// directory read, and an added target re-runs this test.
	declared, err := fuzzTargets(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	if len(declared) == 0 {
		t.Fatal("no Fuzz targets found in the module; has the scan broken?")
	}
	if !slices.Equal(inCI, declared) {
		t.Errorf("%q fuzzes %v, but the module declares %v", fuzzStep, inCI, declared)
	}
}
