package tanktemp_test

// The checks behind ci_test.go, as pure functions: each takes bytes, a
// string or an fs.FS and returns values and errors, with no testing.T and no
// disk. cicheck_table_test.go drives them with fixtures; ci_test.go runs them
// on the real repo. Why the step is shaped this way is in GUESSES G52–G53.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// minFuzzExecs is the smallest -fuzztime count the step may use, so it
	// can't be cut to fuzzing next to nothing (G52).
	minFuzzExecs = 100_000
	// minFuzzStepMinutes and maxFuzzStepMinutes bound timeout-minutes (G52).
	minFuzzStepMinutes = 1
	maxFuzzStepMinutes = 15
)

// step is just enough of a GitHub Actions step.
type step struct {
	Name           string `yaml:"name"`
	Run            string `yaml:"run"`
	TimeoutMinutes int    `yaml:"timeout-minutes"`
}

// findStep returns the one step called name in a workflow. Not finding it,
// or finding it twice, is an error: either would let every other check pass
// on the wrong thing. A timeout-minutes that isn't an integer (an Actions
// expression) fails to decode, loudly, rather than reading as no timeout.
func findStep(workflowYAML []byte, name string) (step, error) {
	var wf struct {
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowYAML, &wf); err != nil {
		return step{}, err
	}
	var found []step
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if s.Name == name {
				found = append(found, s)
			}
		}
	}
	if len(found) != 1 {
		return step{}, fmt.Errorf("want exactly one step named %q, found %d", name, len(found))
	}
	return found[0], nil
}

// checkTimeout says whether s has a timeout-minutes within bounds. YAML's
// zero value and a missing key look the same, and both mean GitHub's 6 h.
func checkTimeout(s step) error {
	switch {
	case s.TimeoutMinutes < minFuzzStepMinutes:
		return fmt.Errorf("%q has no timeout-minutes; a count has no deadline, so a stuck worker would hold the job for 6 h", s.Name)
	case s.TimeoutMinutes > maxFuzzStepMinutes:
		return fmt.Errorf("%q has timeout-minutes %d; want at most %d", s.Name, s.TimeoutMinutes, maxFuzzStepMinutes)
	}
	return nil
}

var (
	// forLoop matches `for v in LIST; do BODY done`, with `do` after a ';' or
	// on its own line. Line continuations are joined first.
	forLoop = regexp.MustCompile(`(?s)\bfor\s+(\w+)\s+in\s+([^;\n]*?)\s*(?:;|\n)\s*do\s(.*?)\bdone\b`)
	// fuzzCmd matches one go test line that fuzzes: -fuzz followed by '=' or
	// a space, so -fuzztime alone doesn't count.
	fuzzCmd = regexp.MustCompile(`(?m)^.*\bgo\s+test\b.*\s-fuzz[=\s].*$`)
	// fuzzArg and fuzzTimeArg pick out a flag's value, after '=' or any
	// whitespace, with or without quotes.
	fuzzArg     = regexp.MustCompile(`\s-fuzz(?:=|\s+)(?:"([^"]*)"|'([^']*)'|(\S+))`)
	fuzzTimeArg = regexp.MustCompile(`\s-fuzztime(?:=|\s+)(?:"([^"]*)"|'([^']*)'|(\S+))`)
	// execCount is what go test accepts as a count: digits then 'x'.
	execCount = regexp.MustCompile(`^([0-9]+)x$`)
	// pkgArg is a ./relative package argument, quoted or not.
	pkgArg = regexp.MustCompile(`^["']?(\.(?:/[^\s"']*)?)["']?$`)
)

// value is whichever of a quoted-or-bare submatch matched.
func value(m []string) string {
	return m[1] + m[2] + m[3]
}

// stepTargets returns every "dir:Target" a step's run script fuzzes, sorted,
// with every problem found in how it fuzzes them. dir is the package as go
// test is given it, less "./"; the module root is ".".
//
// The script must be one or more `for t in TARGETS; do … done` loops, each
// with go test lines that fuzz "$t" in one ./package with a -fuzztime count.
// Anything else it can't pair with a package and target is an error rather
// than skipped, so a reshaped step fails here instead of fuzzing nothing.
func stepTargets(run string) ([]string, []error) {
	run = strings.ReplaceAll(run, "\\\n", " ")
	var targets []string
	var errs []error
	inLoops := 0
	for _, loop := range forLoop.FindAllStringSubmatch(run, -1) {
		v, items, body := loop[1], strings.Fields(loop[2]), loop[3]
		for _, cmd := range fuzzCmd.FindAllString(body, -1) {
			inLoops++
			dir, err := checkFuzzCommand(cmd, v)
			if err != nil {
				errs = append(errs, err)
				if dir == "" {
					continue
				}
			}
			for _, it := range items {
				targets = append(targets, dir+":"+it)
			}
		}
	}
	all := fuzzCmd.FindAllString(run, -1)
	if len(all) == 0 {
		return nil, []error{errors.New("no fuzz loop: want `for t in Fuzz…; do go test -fuzz \"^$t\\$\" … done`")}
	}
	if n := len(all) - inLoops; n > 0 {
		errs = append(errs, fmt.Errorf("%d fuzz command(s) outside a for loop; the check can't tell what they fuzz", n))
	}
	slices.Sort(targets)
	return targets, errs
}

// checkFuzzCommand checks one fuzzing go test line from a loop over v, and
// returns its package dir. The dir is returned even with an error when the
// package itself was clear, so the targets still count toward coverage.
func checkFuzzCommand(cmd, v string) (string, error) {
	cmd = " " + cmd
	var problems []string

	f := fuzzArg.FindStringSubmatch(cmd)
	if f == nil || !(strings.Contains(value(f), "$"+v) || strings.Contains(value(f), "${"+v+"}")) {
		problems = append(problems, "-fuzz doesn't use $"+v)
	}

	switch ts := fuzzTimeArg.FindAllStringSubmatch(cmd, -1); len(ts) {
	case 0:
		problems = append(problems, "no -fuzztime: it would run until the step times out")
	case 1:
		if p := checkFuzzTime(value(ts[0])); p != "" {
			problems = append(problems, p)
		}
	default:
		problems = append(problems, fmt.Sprintf("%d -fuzztime flags; want one", len(ts)))
	}

	var pkgs []string
	for _, field := range strings.Fields(cmd) {
		if m := pkgArg.FindStringSubmatch(field); m != nil {
			pkgs = append(pkgs, m[1])
		}
	}
	dir := ""
	if len(pkgs) == 1 && !strings.Contains(pkgs[0], "...") {
		dir = path.Clean(pkgs[0])
	} else {
		problems = append(problems, fmt.Sprintf("want one ./package, not %q: go test -fuzz fuzzes exactly one", pkgs))
	}

	if problems != nil {
		return dir, fmt.Errorf("%s: %s", strings.TrimSpace(cmd), strings.Join(problems, "; "))
	}
	return dir, nil
}

// checkFuzzTime explains what's wrong with a -fuzztime value, or returns "".
// A duration can end in "context deadline exceeded" with nothing wrong
// (issue #5); a variable can't be checked.
func checkFuzzTime(v string) string {
	m := execCount.FindStringSubmatch(v)
	if m == nil {
		return fmt.Sprintf("-fuzztime %s: want an execution count like 1000000x, not a duration (issue #5)", v)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < minFuzzExecs {
		return fmt.Sprintf("-fuzztime %s: at least %dx", v, minFuzzExecs)
	}
	return ""
}

// skipDir reports whether go test ./... would ignore a directory: testdata,
// vendor, and names starting with '.' or '_'.
func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// fuzzTargets returns every "dir:Target" declared in fsys's test files,
// sorted, as the default build context sees them. That's what CI's go test
// compiles: a target behind a build tag CI doesn't set (or for another GOOS)
// can't be fuzzed there, so it isn't one, and isn't required in the loop
// (where it would match nothing and pass silently). Nested modules are left
// to their own CI.
func fuzzTargets(fsys fs.FS) ([]string, error) {
	// Every file read goes through fsys. With GOROOT and GOPATH empty,
	// ImportDir of a local directory never looks anywhere else, so the
	// HasSubdir hook (used only to place a directory under one of them) is
	// never called and isn't set.
	ctxt := build.Default
	ctxt.GOROOT, ctxt.GOPATH = "", ""
	ctxt.JoinPath = path.Join
	ctxt.IsAbsPath = path.IsAbs
	ctxt.IsDir = func(p string) bool {
		fi, err := fs.Stat(fsys, p)
		return err == nil && fi.IsDir()
	}
	ctxt.ReadDir = func(dir string) ([]fs.FileInfo, error) {
		es, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return nil, err
		}
		fis := make([]fs.FileInfo, 0, len(es))
		for _, e := range es {
			fi, err := e.Info()
			if err != nil {
				return nil, err
			}
			fis = append(fis, fi)
		}
		return fis, nil
	}
	ctxt.OpenFile = func(p string) (io.ReadCloser, error) { return fsys.Open(p) }

	var targets []string
	err := fs.WalkDir(fsys, ".", func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if dir != "." {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			if _, err := fs.Stat(fsys, path.Join(dir, "go.mod")); err == nil {
				return fs.SkipDir
			}
		}
		p, err := ctxt.ImportDir(dir, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, name := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
			ts, err := fileFuzzTargets(fsys, path.Join(dir, name))
			if err != nil {
				return err
			}
			for _, t := range ts {
				targets = append(targets, dir+":"+t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(targets)
	return targets, nil
}

// fileFuzzTargets returns the fuzz targets one test file declares: a
// top-level, non-generic func named Fuzz or FuzzX (X not lower case, as go
// test requires), with one *testing.F parameter and no results.
func fileFuzzTargets(fsys fs.FS, name string) ([]string, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	// How "testing" is named in this file: "testing", an alias, or "." for a
	// dot import. "" if it isn't imported, so no *X.F can be testing's.
	testingName := ""
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "testing" {
			testingName = "testing"
			if imp.Name != nil {
				testingName = imp.Name.Name
			}
		}
	}
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Type.TypeParams == nil && fn.Type.Results == nil &&
			isFuzzName(fn.Name.Name) && takesTestingF(fn, testingName) {
			names = append(names, fn.Name.Name)
		}
	}
	return names, nil
}

// isFuzzName is go test's rule: "Fuzz", then nothing or a rune that isn't
// lower case. Fuzzy isn't a target; Fuzz, FuzzX and Fuzz_x are.
func isFuzzName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Fuzz")
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// takesTestingF reports whether fn's only parameter is a *testing.F, with
// testing named as the file imports it.
func takesTestingF(fn *ast.FuncDecl, testingName string) bool {
	ps := fn.Type.Params.List
	if testingName == "" || len(ps) != 1 || len(ps[0].Names) > 1 {
		return false
	}
	star, ok := ps[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := star.X.(type) {
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == testingName && x.Sel.Name == "F"
	case *ast.Ident:
		return testingName == "." && x.Name == "F"
	}
	return false
}
