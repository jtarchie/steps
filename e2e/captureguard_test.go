package e2e

// The rule that keeps stdout capture from becoming a data race.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoParallelTestRedirectsStdout enforces a convention four packages state
// in a comment and nothing checked.
//
// Redirecting output means assigning the os.Stdout GLOBAL, which every
// fmt.Printf in the code under test reads — so a redirect running beside a
// parallel test that prints is a data race, not merely interleaved output. It
// has never fired here for a structural reason rather than a careful one: no
// redirecting test is parallel today, and Go does not overlap a sequential
// test's body with a paused parallel one. Add t.Parallel() to any of the forty
// or so redirecting tests and the race is live, intermittently, in a suite
// where an intermittent failure reads as noise and gets re-run rather than
// read. That is exactly how it went unnoticed in internal/trigger until a
// full-suite run caught it four times and three clean ones could not.
//
// internal/trigger is deliberately absent from what this finds: its capture
// swaps that package's OWN writer under a lock, so a parallel test may use it
// and one does. That is the upgrade path for any package this test starts
// failing for — give the package a writer of its own rather than deleting the
// t.Parallel() and paying for it in wall clock.
//
// Keyed on what a function DOES, never on what it is called: three of the four
// helpers happen to be named captureStdout and the fourth is inline in a test,
// so a name-based rule would have covered the ones already documented and
// missed the one that was not.
func TestNoParallelTestRedirectsStdout(t *testing.T) {
	t.Parallel()

	packages := parseTestPackages(t)

	// A guard that finds nothing to guard has stopped guarding.
	total := 0

	for _, pkg := range packages {
		total += len(pkg.redirectors)
	}

	if total == 0 {
		t.Fatal("no function assigning os.Stdout was found — this check no longer checks anything")
	}

	for _, pkg := range packages {
		for _, offender := range pkg.parallelRedirectors() {
			t.Errorf("%s is parallel AND redirects os.Stdout — that is a data race, not a flake", offender)
		}
	}
}

// testPackage is one directory's test files, and which of their functions
// reassign os.Stdout.
type testPackage struct {
	dir         string
	tests       map[string]*ast.FuncDecl
	redirectors map[string]bool
}

// parallelRedirectors names the tests in this package that are parallel and
// reach a reassignment of os.Stdout, directly or through one of its helpers.
func (p testPackage) parallelRedirectors() []string {
	var offenders []string

	for name, fn := range p.tests {
		if !calls(fn.Body, "Parallel") {
			continue
		}

		if !assignsStdout(fn.Body) && !callsAny(fn.Body, p.redirectors) {
			continue
		}

		offenders = append(offenders, p.dir+":"+name)
	}

	return offenders
}

// parseTestPackages reads every _test.go file in the module, grouped by
// directory.
func parseTestPackages(t *testing.T) []testPackage {
	t.Helper()

	byDir := map[string]*testPackage{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr // an unreadable path is not this test's business
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil //nolint:nilerr // as above
		}

		dir := filepath.Dir(path)

		pkg, ok := byDir[dir]
		if !ok {
			pkg = &testPackage{dir: dir, tests: map[string]*ast.FuncDecl{}, redirectors: map[string]bool{}}
			byDir[dir] = pkg
		}

		collectFuncs(file, pkg)

		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}

	packages := make([]testPackage, 0, len(byDir))
	for _, pkg := range byDir {
		packages = append(packages, *pkg)
	}

	return packages
}

// collectFuncs records one file's tests and its stdout-reassigning helpers.
func collectFuncs(file *ast.File, pkg *testPackage) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		if strings.HasPrefix(fn.Name.Name, "Test") {
			pkg.tests[fn.Name.Name] = fn
		}

		if assignsStdout(fn.Body) && !strings.HasPrefix(fn.Name.Name, "Test") {
			pkg.redirectors[fn.Name.Name] = true
		}
	}
}

// assignsStdout reports whether body reassigns os.Stdout.
func assignsStdout(body *ast.BlockStmt) bool {
	found := false

	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}

		for _, target := range assign.Lhs {
			if selectorText(target) == "os.Stdout" {
				found = true
			}
		}

		return true
	})

	return found
}

// callsAny reports whether body calls any of the named functions.
func callsAny(body *ast.BlockStmt, names map[string]bool) bool {
	for name := range names {
		if calls(body, name) {
			return true
		}
	}

	return false
}

// calls reports whether body calls a function with this name, on any receiver.
func calls(body *ast.BlockStmt, name string) bool {
	found := false

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				found = true
			}
		}

		return true
	})

	return found
}

// selectorText renders a qualified identifier, for comparing against
// "os.Stdout" without hand-walking it.
func selectorText(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}

	ident, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}

	return ident.Name + "." + selector.Sel.Name
}
