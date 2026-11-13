package tanktemp_test

// Table tests for the checks in cicheck_test.go. Every row is a fixture, so
// each failure path is exercised without touching the real ci.yml or the
// real test files. A new corner case is a new row.

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// wantErr checks err against want, a substring; "" means no error.
func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("unexpected error: %v", err)
	case want != "" && err == nil:
		t.Errorf("no error, want one containing %q", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("error %q, want one containing %q", err, want)
	}
}

// wantErrs is wantErr for a list: want is the substrings expected, one per
// error, in order.
func wantErrs(t *testing.T, errs []error, want []string) {
	t.Helper()
	if len(errs) != len(want) {
		t.Errorf("got %d errors %v, want %d containing %q", len(errs), errs, len(want), want)
		return
	}
	for i := range errs {
		wantErr(t, errs[i], want[i])
	}
}

func TestFindStep(t *testing.T) {
	const name = "fuzz (short)"
	for _, tc := range []struct {
		what    string
		yaml    string
		want    step
		wantErr string
	}{
		{
			what: "one step, found by name",
			yaml: `
jobs:
  test:
    steps:
      - name: vet
        run: go vet ./...
      - name: fuzz (short)
        timeout-minutes: 10
        run: echo fuzz
`,
			want: step{Name: name, Run: "echo fuzz", TimeoutMinutes: 10},
		},
		{
			what: "found in any job",
			yaml: `
jobs:
  build:
    steps: [{name: build, run: go build}]
  test:
    steps: [{name: fuzz (short), run: x}]
`,
			want: step{Name: name, Run: "x"},
		},
		{
			what:    "missing: a rename can't make every other check pass vacuously",
			yaml:    "jobs:\n  test:\n    steps: [{name: fuzz, run: x}]\n",
			wantErr: "found 0",
		},
		{
			what: "twice, in two jobs: which one is CI's is ambiguous",
			yaml: `
jobs:
  a: {steps: [{name: fuzz (short), run: x}]}
  b: {steps: [{name: fuzz (short), run: y}]}
`,
			wantErr: "found 2",
		},
		{
			what:    "an expression timeout, valid in Actions, is refused loudly, not read as 0",
			yaml:    "jobs:\n  test:\n    steps: [{name: fuzz (short), timeout-minutes: '${{ inputs.t }}', run: x}]\n",
			wantErr: "cannot unmarshal",
		},
		{
			what:    "not YAML",
			yaml:    "jobs: [",
			wantErr: "yaml",
		},
		{
			what:    "no jobs at all",
			yaml:    "name: ci\n",
			wantErr: "found 0",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := findStep([]byte(tc.yaml), name)
			wantErr(t, err, tc.wantErr)
			if tc.wantErr == "" && got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCheckTimeout(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		wantErr string
	}{
		{0, "no timeout-minutes"},
		{-1, "no timeout-minutes"},
		{minFuzzStepMinutes, ""},
		{10, ""},
		{maxFuzzStepMinutes, ""},
		{maxFuzzStepMinutes + 1, "at most"},
		{360, "at most"}, // GitHub's default: the thing the timeout is for
	} {
		t.Run(fmt.Sprint(tc.minutes), func(t *testing.T) {
			wantErr(t, checkTimeout(step{Name: "fuzz", TimeoutMinutes: tc.minutes}), tc.wantErr)
		})
	}
}

// loop is the step's canonical shape, with the go test line swapped in.
func loop(targets, cmd string) string {
	return "for t in " + targets + "; do\n  " + cmd + "\ndone\n"
}

const goodCmd = `go test -run '^$' -fuzz "^$t\$" -fuzztime 1000000x ./internal/onewire/`

// Targets are "dir:Name", dir as go test is given it, less "./": the module
// root is ".".
func TestStepTargets(t *testing.T) {
	for _, tc := range []struct {
		what     string
		run      string
		want     []string
		wantErrs []string
	}{
		{
			what: "the step as written",
			run:  loop("FuzzA FuzzB", goodCmd),
			want: []string{"internal/onewire:FuzzA", "internal/onewire:FuzzB"},
		},
		{
			what: "-fuzztime=N form",
			run:  loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime=1000000x ./internal/onewire/`),
			want: []string{"internal/onewire:FuzzA"},
		},
		{
			what: "${t}, and a package path with no trailing slash",
			run:  loop("FuzzA", `go test -fuzz "^${t}\$" -fuzztime 1000000x ./internal/onewire`),
			want: []string{"internal/onewire:FuzzA"},
		},
		{
			what: "the floor itself is enough",
			run:  loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 100000x ./p/`),
			want: []string{"p:FuzzA"},
		},
		{
			what: "the list continued over lines with a backslash",
			run:  "for t in FuzzA \\\n    FuzzB; do\n  " + goodCmd + "\ndone\n",
			want: []string{"internal/onewire:FuzzA", "internal/onewire:FuzzB"},
		},
		{
			what: "do on its own line",
			run:  "for t in FuzzA FuzzB\ndo\n  " + goodCmd + "\ndone\n",
			want: []string{"internal/onewire:FuzzA", "internal/onewire:FuzzB"},
		},
		{
			what: "the go test line continued with a backslash",
			run:  loop("FuzzA", "go test -fuzz \"^$t\\$\" \\\n    -fuzztime 1000000x ./p/"),
			want: []string{"p:FuzzA"},
		},
		{
			what: "one loop per package",
			run: loop("FuzzA", goodCmd) +
				loop("FuzzC", `go test -run '^$' -fuzz "^$t\$" -fuzztime 1000000x ./internal/payload/`),
			want: []string{"internal/onewire:FuzzA", "internal/payload:FuzzC"},
		},
		{
			what: "a loop with no fuzz command isn't a fuzz loop",
			run:  "for d in a b; do echo $d; done\n" + loop("FuzzA", goodCmd),
			want: []string{"internal/onewire:FuzzA"},
		},
		{
			what: "the module root as the package",
			run:  loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 1000000x ./`),
			want: []string{".:FuzzA"},
		},

		// -fuzztime: a duration, in every spelling, races the deadline (#5).
		{
			what:     "a duration",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 20s ./p/`),
			wantErrs: []string{"-fuzztime 20s: want an execution count"},
		},
		{
			what:     "a duration after =",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime=20s ./p/`),
			wantErrs: []string{"-fuzztime 20s"},
		},
		{
			what:     "a duration after a tab",
			run:      loop("FuzzA", "go test -fuzz \"^$t\\$\" -fuzztime\t20s ./p/"),
			wantErrs: []string{"-fuzztime 20s"},
		},
		{
			what:     "a quoted duration",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime '20s' ./p/`),
			wantErrs: []string{"-fuzztime 20s"},
		},
		{
			what:     "a variable: can't be checked, so refused",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime "$FT" ./p/`),
			wantErrs: []string{"-fuzztime $FT"},
		},
		{
			what:     "0x",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 0x ./p/`),
			wantErrs: []string{"-fuzztime 0x"},
		},
		{
			what:     "1x fuzzes next to nothing",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 1x ./p/`),
			wantErrs: []string{"-fuzztime 1x: at least 100000x"},
		},
		{
			what:     "just under the floor",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 99999x ./p/`),
			wantErrs: []string{"at least 100000x"},
		},
		{
			what:     "a count too big for an int is refused, not wrapped",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 99999999999999999999x ./p/`),
			wantErrs: []string{"at least 100000x"},
		},
		{
			what:     "no -fuzztime runs until the step times out",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" ./p/`),
			wantErrs: []string{"no -fuzztime"},
		},
		{
			what:     "two -fuzztimes: the last wins, so which is meant is unclear",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 1000000x -fuzztime 20s ./p/`),
			wantErrs: []string{"2 -fuzztime"},
		},
		{
			what: "a second fuzz command in the loop with no -fuzztime",
			run: "for t in FuzzA; do\n  " + goodCmd +
				"\n  go test -fuzz \"^$t\\$\" ./internal/other/\ndone\n",
			want:     []string{"internal/onewire:FuzzA", "internal/other:FuzzA"},
			wantErrs: []string{"no -fuzztime"},
		},

		// Shape: anything the check can't pair with a package and a target.
		{
			what:     "a fuzz command outside any loop",
			run:      loop("FuzzA", goodCmd) + `go test -fuzz '^FuzzB$' -fuzztime 1000000x ./p/` + "\n",
			want:     []string{"internal/onewire:FuzzA"},
			wantErrs: []string{"outside a for loop"},
		},
		{
			what:     "-fuzz that doesn't use the loop variable",
			run:      loop("FuzzA", `go test -fuzz '^FuzzA$' -fuzztime 1000000x ./p/`),
			wantErrs: []string{"doesn't use $t"},
		},
		{
			what:     "no package argument: go test fuzzes the working directory",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 1000000x`),
			wantErrs: []string{"want one ./package"},
		},
		{
			what:     "two package arguments: go test refuses -fuzz on more than one",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 1000000x ./a/ ./b/`),
			wantErrs: []string{"want one ./package"},
		},
		{
			what:     "./... matches several packages",
			run:      loop("FuzzA", `go test -fuzz "^$t\$" -fuzztime 1000000x ./...`),
			wantErrs: []string{"want one ./package"},
		},
		{
			what:     "nothing fuzzed at all",
			run:      "echo nothing\n",
			wantErrs: []string{"no fuzz loop"},
		},
		{
			what:     "empty",
			run:      "",
			wantErrs: []string{"no fuzz loop"},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, errs := stepTargets(tc.run)
			wantErrs(t, errs, tc.wantErrs)
			if tc.want != nil || len(tc.wantErrs) == 0 {
				if !slices.Equal(got, tc.want) {
					t.Errorf("targets %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// src is a test file in package pkg with the given body.
func src(pkg, body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("package " + pkg + "\n\nimport \"testing\"\n\n" + body + "\n")}
}

const fuzzA = "func FuzzA(f *testing.F) {}"

func TestFuzzTargets(t *testing.T) {
	for _, tc := range []struct {
		what    string
		fsys    fs.FS
		want    []string
		wantErr string
	}{
		{
			what: "one target",
			fsys: fstest.MapFS{"internal/a/a_test.go": src("a", fuzzA)},
			want: []string{"internal/a:FuzzA"},
		},
		{
			what: "every package in the module, sorted",
			fsys: fstest.MapFS{
				"internal/b/b_test.go": src("b", "func FuzzB(f *testing.F) {}\nfunc FuzzA(f *testing.F) {}"),
				"internal/a/a_test.go": src("a", fuzzA),
				"x_test.go":            src("x", "func FuzzRoot(f *testing.F) {}"),
			},
			want: []string{".:FuzzRoot", "internal/a:FuzzA", "internal/b:FuzzA", "internal/b:FuzzB"},
		},
		{
			what: "an external test package is the same package to go test",
			fsys: fstest.MapFS{
				"a/a.go":      {Data: []byte("package a\n")},
				"a/x_test.go": src("a_test", fuzzA),
			},
			want: []string{"a:FuzzA"},
		},
		{
			what: "Fuzz alone is a valid name",
			fsys: fstest.MapFS{"a/a_test.go": src("a", "func Fuzz(f *testing.F) {}")},
			want: []string{"a:Fuzz"},
		},
		{
			what: "FuzzX_y and Fuzz_x are valid names",
			fsys: fstest.MapFS{"a/a_test.go": src("a", "func FuzzX_y(f *testing.F) {}\nfunc Fuzz_x(f *testing.F) {}")},
			want: []string{"a:FuzzX_y", "a:Fuzz_x"},
		},
		{
			what: "an aliased testing import",
			fsys: fstest.MapFS{"a/a_test.go": {Data: []byte("package a\n\nimport tt \"testing\"\n\nfunc FuzzA(f *tt.F) {}\n")}},
			want: []string{"a:FuzzA"},
		},
		{
			what: "a dot import of testing",
			fsys: fstest.MapFS{"a/a_test.go": {Data: []byte("package a\n\nimport . \"testing\"\n\nfunc FuzzA(f *F) {}\n")}},
			want: []string{"a:FuzzA"},
		},
		{
			what: "an unnamed parameter",
			fsys: fstest.MapFS{"a/a_test.go": src("a", "func FuzzA(*testing.F) {}")},
			want: []string{"a:FuzzA"},
		},
		{
			what: "not targets: a method, a lowercase next letter, *testing.T, two params, a result",
			fsys: fstest.MapFS{"a/a_test.go": src("a", `
type T struct{}
func (T) FuzzMethod(f *testing.F) {}
func Fuzzy(f *testing.F) {}
func FuzzT(t *testing.T) {}
func FuzzTwo(f *testing.F, n int) {}
func FuzzTwoNames(f, g *testing.F) {}
func FuzzResult(f *testing.F) error { return nil }
func FuzzValue(f testing.F) {}
func FuzzGeneric[T any](f *testing.F) {}
`)},
			want: nil,
		},
		{
			what: "a name that doesn't start with Fuzz, even with a *testing.F",
			fsys: fstest.MapFS{"a/a_test.go": src("a", "func helper(f *testing.F) {}\nfunc TestFuzzA(f *testing.F) {}")},
			want: nil,
		},
		{
			what: "a pointer to something that's neither testing.F nor F",
			fsys: fstest.MapFS{"a/a_test.go": src("a", "type G[T any] struct{}\nfunc FuzzSlice(f *[]int) {}\nfunc FuzzIndexed(f *G[int]) {}")},
			want: nil,
		},
		{
			what: "*F from a package that isn't testing",
			fsys: fstest.MapFS{"a/a_test.go": {Data: []byte("package a\n\nimport testing \"example.com/notesting\"\n\nfunc FuzzA(f *testing.F) {}\n")}},
			want: nil,
		},
		{
			what: "a non-test file isn't compiled by go test -fuzz",
			fsys: fstest.MapFS{"a/a.go": src("a", fuzzA)},
			want: nil,
		},
		{
			// The bug review on #6 found: CI runs without -tags, so a tagged
			// target in the loop would match nothing and pass silently.
			what: "behind a build tag CI doesn't set",
			fsys: fstest.MapFS{
				"a/a_test.go": src("a", fuzzA),
				"a/hw_test.go": {Data: []byte("//go:build hardware\n\npackage a\n\nimport \"testing\"\n\n" +
					"func FuzzHW(f *testing.F) {}\n")},
			},
			want: []string{"a:FuzzA"},
		},
		{
			what: "only tagged files: no package at all in the default build",
			fsys: fstest.MapFS{
				"a/hw_test.go": {Data: []byte("//go:build hardware\n\npackage a\n\nimport \"testing\"\n\n" + fuzzA + "\n")},
			},
			want: nil,
		},
		{
			what: "a GOOS CI isn't",
			fsys: fstest.MapFS{"a/a_plan9_test.go": src("a", fuzzA)},
			want: nil,
		},
		{
			what: "directories go test ignores: testdata, dot and underscore, vendor",
			fsys: fstest.MapFS{
				"a/testdata/x_test.go": src("x", fuzzA),
				".git/x_test.go":       src("x", fuzzA),
				"_old/x_test.go":       src("x", fuzzA),
				"vendor/v/x_test.go":   src("x", fuzzA),
			},
			want: nil,
		},
		{
			what: "a nested module is another module's CI",
			fsys: fstest.MapFS{
				"sub/go.mod":       {Data: []byte("module example.com/sub\n")},
				"sub/x/x_test.go":  src("x", fuzzA),
				"main/m_test.go":   src("m", "func FuzzM(f *testing.F) {}"),
				"sub/sub_test.go":  src("sub", fuzzA),
				"main/m/x_test.go": src("x", "func FuzzN(f *testing.F) {}"),
			},
			want: []string{"main/m:FuzzN", "main:FuzzM"},
		},
		{
			what:    "a test file that doesn't parse",
			fsys:    fstest.MapFS{"a/a_test.go": {Data: []byte("package a\n\nfunc FuzzA(f *testing.F {\n")}},
			wantErr: "a/a_test.go",
		},
		{
			what:    "two packages in one directory",
			fsys:    fstest.MapFS{"a/a_test.go": src("a", fuzzA), "a/b_test.go": src("b", "")},
			wantErr: "found packages",
		},
		{
			// go/build lists a directory before the walk does, so failing only
			// the first listing pins go/build's error path, not the walk's.
			what:    "a directory that can't be listed",
			fsys:    failFS{MapFS: fstest.MapFS{"a/a_test.go": src("a", fuzzA)}, readDir: "a", listed: map[string]int{}},
			wantErr: "failFS: ReadDir a",
		},
		{
			what:    "a file whose info can't be read",
			fsys:    failFS{MapFS: fstest.MapFS{"a/a_test.go": src("a", fuzzA)}, info: "a_test.go"},
			wantErr: "failFS: Info a_test.go",
		},
		{
			// go/build opens the file through fsys.Open, so only the second
			// read, the whole file for the parser, fails.
			what:    "a test file that can't be read",
			fsys:    failFS{MapFS: fstest.MapFS{"a/a_test.go": src("a", fuzzA)}, readFile: "a/a_test.go"},
			wantErr: "failFS: ReadFile a/a_test.go",
		},
		{
			what: "an empty tree",
			fsys: fstest.MapFS{},
			want: nil,
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got, err := fuzzTargets(tc.fsys)
			wantErr(t, err, tc.wantErr)
			if tc.wantErr == "" && !slices.Equal(got, tc.want) {
				t.Errorf("targets %v, want %v", got, tc.want)
			}
		})
	}
}

// failFS is a hand-written fake: an fstest.MapFS that fails one named
// operation, for the I/O error paths a MapFS can't reach. Everything else
// goes to the MapFS.
type failFS struct {
	fstest.MapFS
	readDir  string         // the first fs.ReadDir of this directory fails
	listed   map[string]int // ReadDir calls so far, by directory
	info     string         // Info of the entry with this base name fails
	readFile string         // fs.ReadFile of this file fails (fsys.Open still works)
}

func (f failFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.listed != nil {
		f.listed[name]++
	}
	if name == f.readDir && f.listed[name] == 1 {
		return nil, errors.New("failFS: ReadDir " + name)
	}
	es, err := f.MapFS.ReadDir(name)
	for i, e := range es {
		if e.Name() == f.info {
			es[i] = failInfo{e}
		}
	}
	return es, err
}

func (f failFS) ReadFile(name string) ([]byte, error) {
	if name == f.readFile {
		return nil, errors.New("failFS: ReadFile " + name)
	}
	return f.MapFS.ReadFile(name)
}

// failInfo is a DirEntry whose Info fails.
type failInfo struct{ fs.DirEntry }

func (e failInfo) Info() (fs.FileInfo, error) {
	return nil, errors.New("failFS: Info " + e.Name())
}
