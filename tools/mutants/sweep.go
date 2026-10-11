package main

// sweep: gremlins' measurement without gremlins' cost. gremlins runs a package's WHOLE suite, docker included, against every mutant, one at a time in packages that touch docker — venue took 11 hours. A test that never executes a statement cannot notice it changing, so each mutant here runs only the tests whose own coverage profile executes it: the ones that need no docker daemon several at a time, the docker ones afterwards and serially, for the survivors only. The report is gremlins' JSON shape, so `record` and the mutation-sweep skill read it unchanged.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// operators are gremlins v0.6.0's DEFAULT-enabled mutators and its token mappings for them, so a ledger row means what it meant when gremlins measured it. A SUB gets two mutants that make the same change, because gremlins counts both.
var operators = map[token.Token][]struct { //nolint:gochecknoglobals // a table
	kind string
	to   token.Token
}{
	token.ADD: {{"ARITHMETIC_BASE", token.SUB}},
	token.SUB: {{"INVERT_NEGATIVES", token.ADD}, {"ARITHMETIC_BASE", token.ADD}},
	token.MUL: {{"ARITHMETIC_BASE", token.QUO}},
	token.QUO: {{"ARITHMETIC_BASE", token.MUL}},
	token.REM: {{"ARITHMETIC_BASE", token.MUL}},
	token.EQL: {{"CONDITIONALS_NEGATION", token.NEQ}},
	token.NEQ: {{"CONDITIONALS_NEGATION", token.EQL}},
	token.LSS: {{"CONDITIONALS_BOUNDARY", token.LEQ}, {"CONDITIONALS_NEGATION", token.GEQ}},
	token.LEQ: {{"CONDITIONALS_BOUNDARY", token.LSS}, {"CONDITIONALS_NEGATION", token.GTR}},
	token.GTR: {{"CONDITIONALS_BOUNDARY", token.GEQ}, {"CONDITIONALS_NEGATION", token.LEQ}},
	token.GEQ: {{"CONDITIONALS_BOUNDARY", token.GTR}, {"CONDITIONALS_NEGATION", token.LSS}},
	token.INC: {{"INCREMENT_DECREMENT", token.DEC}},
	token.DEC: {{"INCREMENT_DECREMENT", token.INC}},
}

// noDocker is what a test process sees in the parallel tier: testdocker.Require skips every docker test against it, which is how a test is classified without anyone keeping a list.
const noDocker = "DOCKER_HOST=unix:///nonexistent.sock"

// coverage is one test's executed blocks, by file NAME: a sweep is one directory, and a profile names files by import path, which only the module knows how to map back.
type coverage map[string][]span

type testRun struct {
	name    string
	seconds float64
	skipped bool
	docker  bool
	covered coverage
}

// sweepPackage measures one package and writes its gremlins-shaped report, returning the report's path.
func sweepPackage(ctx context.Context, dir string, workers int, since string) (string, error) {
	files, err := packageFiles(ctx, dir, since)
	if err != nil {
		return "", err
	}

	work, err := os.MkdirTemp("", "mutants-sweep-")
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	start := time.Now()

	tests, err := profileTests(ctx, dir, work, workers)
	if err != nil {
		return "", err
	}

	mutants, err := mutantsOf(files, tests)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "%s: %d tests profiled in %s, %d mutants\n", dir, len(tests), time.Since(start).Round(time.Second), len(mutants))

	judgeAll(ctx, dir, work, mutants, workers, tests)

	report := filepath.Join(".gremlins", strings.ReplaceAll(strings.TrimPrefix(dir, "./"), "/", "_")+".json")

	err = writeReport(report, dir, mutants, time.Since(start))
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "%s: done in %s, report %s\n", dir, time.Since(start).Round(time.Second), report)

	return report, nil
}

// judgeAll runs the docker-free tier workers at a time, then the docker tier serially for what survived it; a mutant neither ran is NOT COVERED.
func judgeAll(ctx context.Context, dir, work string, mutants []mutant, workers int, tests []testRun) {
	seconds := map[string]float64{}
	for _, test := range tests {
		seconds[test.name] = test.seconds
	}

	runTier(ctx, dir, work, mutants, workers, seconds, func(m *mutant) []string { return m.fast })
	runTier(ctx, dir, work, mutants, 1, seconds, func(m *mutant) []string {
		if m.Outcome != "LIVED" && m.Outcome != "" {
			return nil
		}

		return m.slow
	})

	for i := range mutants {
		if mutants[i].Outcome == "" {
			mutants[i].Outcome = "NOT COVERED"
		}
	}
}

// packageFiles is the package's own non-test files — not a subdirectory's, which is its own package with its own row — narrowed to those changed since a rev when one is given.
func packageFiles(ctx context.Context, dir, since string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	var changed map[string]bool

	if since != "" {
		out, err := git(ctx, "diff", "--name-only", since, "--", dir)
		if err != nil {
			return nil, err
		}

		changed = map[string]bool{}
		for _, name := range strings.Fields(out) {
			changed[name] = true
		}
	}

	var files []string

	for _, file := range matches {
		file = filepath.ToSlash(filepath.Clean(file))
		if strings.HasSuffix(file, "_test.go") || (changed != nil && !changed[file]) {
			continue
		}

		files = append(files, file)
	}

	return files, nil
}

// profileTests runs every test of the package in a process of its own with a coverage profile: first all of them, workers at a time, with no docker daemon reachable; then, one at a time and with the real environment, those that skipped for want of one. A test failing unmutated stops the sweep, because a mutant it "kills" would be a lie — and the parallel pass is also the proof that this package's tests survive running beside each other.
func profileTests(ctx context.Context, dir, work string, workers int) ([]testRun, error) {
	binary := filepath.Join(work, "cover.test")

	names, err := listTests(ctx, dir, binary)
	if err != nil {
		return nil, err
	}

	runs := make([]testRun, len(names))

	var (
		failed []string
		mu     sync.Mutex
	)

	profile := func(i int, env []string) {
		run, err := profileOne(ctx, dir, work, binary, names[i], env)

		mu.Lock()
		defer mu.Unlock()

		if err != nil {
			failed = append(failed, err.Error())
		}

		runs[i] = run
	}

	parallel(len(names), workers, func(i, _ int) { profile(i, []string{noDocker}) })

	for i := range runs {
		if runs[i].skipped && len(failed) == 0 {
			profile(i, nil)
			runs[i].docker = true
		}
	}

	if len(failed) > 0 {
		return nil, fmt.Errorf("%s fails unmutated, so no mutant can be judged:\n%s", dir, strings.Join(failed, "\n"))
	}

	return runs, nil
}

// listTests builds the package's test binary with coverage and asks it for its tests, benchmarks left out.
func listTests(ctx context.Context, dir, binary string) ([]string, error) {
	build := exec.CommandContext(ctx, "go", "test", "-c", "-cover", "-covermode=set", "-o", binary, dir) //nolint:gosec // a package directory and a path this command made

	out, err := build.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("building %s with coverage: %w\n%s", dir, err, out)
	}

	list := exec.CommandContext(ctx, binary, "-test.list", ".")
	list.Dir = dir

	out, err = list.Output()
	if err != nil {
		return nil, fmt.Errorf("listing %s's tests: %w", dir, err)
	}

	var names []string

	for name := range strings.FieldsSeq(string(out)) {
		if !strings.HasPrefix(name, "Benchmark") && name != "ok" {
			names = append(names, name)
		}
	}

	return names, nil
}

var skipped = regexp.MustCompile(`(?m)^--- SKIP: (\S+)`) //nolint:gochecknoglobals // compiled once

// profileOne runs one test with a coverage profile. A test that SKIPPED at the top level comes back with no coverage, and in the parallel pass that means it wants docker.
func profileOne(ctx context.Context, dir, work, binary, name string, env []string) (testRun, error) {
	path := filepath.Join(work, "prof", strings.ReplaceAll(name, "/", "_")+".out")

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		return testRun{}, fmt.Errorf("%w", err)
	}

	cmd := exec.CommandContext(ctx, binary, "-test.run", "^"+regexp.QuoteMeta(name)+"$", "-test.v", "-test.count=1", "-test.coverprofile="+path) //nolint:gosec // the binary just built, running a test it listed
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)

	started := time.Now()
	out, err := cmd.CombinedOutput()
	run := testRun{name: name, seconds: time.Since(started).Seconds()}

	if err != nil {
		return run, fmt.Errorf("%s: %w\n%s", name, err, tail(out))
	}

	for _, match := range skipped.FindAllSubmatch(out, -1) {
		if string(match[1]) == name {
			run.skipped = true

			return run, nil
		}
	}

	run.covered, err = readCoverage(path)

	return run, err
}

func tail(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	return strings.Join(lines[max(0, len(lines)-20):], "\n")
}

// readCoverage is a profile's executed blocks; a block run zero times is not coverage.
func readCoverage(path string) (coverage, error) {
	f, err := os.Open(path) //nolint:gosec // a profile this command wrote
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	defer func() { _ = f.Close() }()

	covered := coverage{}
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[2] == "0" { //nolint:mnd // block, statements, count
			continue
		}

		s, err := parseSpan(fields[0])
		if err != nil {
			continue // the "mode:" line
		}

		covered[filepath.Base(s.file)] = append(covered[filepath.Base(s.file)], s)
	}

	return covered, scanner.Err() //nolint:wrapcheck // a scan error says what failed
}

// covers is gremlins' rule, so NOT COVERED means what it did: a line strictly inside a block counts whatever its column, and the first and last lines count from or to the block's edge.
func (s span) covers(p token.Position) bool {
	switch {
	case p.Line > s.startLine && p.Line < s.endLine:
		return true
	case s.startLine == s.endLine:
		return p.Line == s.startLine && p.Column >= s.startCol && p.Column <= s.endCol
	case p.Line == s.startLine:
		return p.Column >= s.startCol
	case p.Line == s.endLine:
		return p.Column <= s.endCol
	}

	return false
}

// mutantsOf finds every default-operator mutant in the files and names, for each, the tests whose profile executes it, split by whether they need docker.
func mutantsOf(files []string, tests []testRun) ([]mutant, error) {
	var found []mutant

	for _, file := range files {
		fset := token.NewFileSet()

		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}

		ast.Inspect(parsed, func(node ast.Node) bool {
			op, pos, ok := operatorOf(node)
			if !ok {
				return true
			}

			at := fset.Position(pos)
			fast, slow := coveringTests(file, at, tests)

			for _, operator := range operators[op] {
				found = append(found, mutant{
					File: file, Line: at.Line, Column: at.Column, offset: at.Offset,
					Type: operator.kind, From: op.String(), To: operator.to.String(), fast: fast, slow: slow,
				})
			}

			return true
		})
	}

	return found, nil
}

// operatorOf is the token a default mutator could change in node, and where it is.
func operatorOf(node ast.Node) (token.Token, token.Pos, bool) {
	switch n := node.(type) {
	case *ast.BinaryExpr:
		return n.Op, n.OpPos, true
	case *ast.UnaryExpr:
		return n.Op, n.OpPos, true
	case *ast.IncDecStmt:
		return n.Tok, n.TokPos, true
	}

	return token.ILLEGAL, token.NoPos, false
}

// coveringTests are the tests whose profile executes at, split by whether they need docker.
func coveringTests(file string, at token.Position, tests []testRun) ([]string, []string) {
	var fast, slow []string

	for _, test := range tests {
		if !slices.ContainsFunc(test.covered[filepath.Base(file)], func(s span) bool { return s.covers(at) }) {
			continue
		}

		if test.docker {
			slow = append(slow, test.name)
		} else {
			fast = append(fast, test.name)
		}
	}

	return fast, slow
}

// runTier runs, workers at a time, every mutant that tests() names tests for, and records the outcome. A mutant it names none for keeps the outcome it had.
func runTier(ctx context.Context, dir, work string, mutants []mutant, workers int, seconds map[string]float64, tests func(*mutant) []string) {
	var queue []int

	for i := range mutants {
		if len(tests(&mutants[i])) > 0 {
			queue = append(queue, i)
		}
	}

	fmt.Fprintf(os.Stderr, "%s: running %d mutants, %d at a time\n", dir, len(queue), workers)

	var done sync.Mutex

	count := 0

	parallel(len(queue), workers, func(i int, worker int) {
		m := &mutants[queue[i]]
		m.Outcome, m.Killers = sweepMutant(ctx, dir, filepath.Join(work, "w"+strconv.Itoa(worker)), *m, tests(m), seconds)

		done.Lock()
		count++
		fmt.Fprintf(os.Stderr, "[%d/%d] %s:%d:%d %s %s %v\n", count, len(queue), m.File, m.Line, m.Column, m.Type, m.Outcome, m.Killers)
		done.Unlock()
	})
}

// sweepMutant builds the package's test binary with the mutant overlaid and runs the named tests, failing fast: killed if any fails.
func sweepMutant(ctx context.Context, dir, work string, m mutant, tests []string, seconds map[string]float64) (string, []string) {
	overlay, err := writeOverlay(work, m)
	if err != nil {
		return "error: " + err.Error(), nil
	}

	binary := filepath.Join(work, "pkg.test")

	build := exec.CommandContext(ctx, "go", "test", "-c", "-overlay", overlay, "-o", binary, dir) //nolint:gosec // paths this command made

	// ponytail: a mutant that does not compile (string + to -) scores KILLED, because gremlins' `go test` failed the same way and every ledger floor was measured so; NOT VIABLE is the honest status once the floors are re-baselined.
	_, err = build.CombinedOutput()
	if err != nil {
		return "KILLED", nil
	}

	// gremlins' coefficient over what just these tests took, plus room for a process to start: the timeout is what stops a mutant that loops — one that allocated without bound reached 174GB under gremlins' whole-suite budget.
	budget := 10 * time.Second
	for _, test := range tests {
		budget += time.Duration(15 * seconds[test] * float64(time.Second)) //nolint:mnd // gremlins' --timeout-coefficient, which this replaces
	}

	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	quoted := make([]string, len(tests))
	for i, test := range tests {
		quoted[i] = regexp.QuoteMeta(test)
	}

	//nolint:gosec // a binary this command built, running tests its profile named
	test := exec.CommandContext(runCtx, binary, "-test.run", "^("+strings.Join(quoted, "|")+")$", "-test.v", "-test.count=1", "-test.failfast")
	test.Dir = dir
	test.WaitDelay = 10 * time.Second
	out, err := test.CombinedOutput()

	switch {
	case runCtx.Err() != nil:
		return "TIMED OUT", nil
	case err == nil:
		return "LIVED", nil
	}

	return "KILLED", failedTests(string(out))
}

// parallel calls fn for 0..n-1, workers at a time, telling it which worker it is so it can have a directory of its own.
func parallel(n, workers int, fn func(i, worker int)) {
	queue := make(chan int)

	var wg sync.WaitGroup

	for worker := range max(workers, 1) {
		wg.Go(func() {
			for i := range queue {
				fn(i, worker)
			}
		})
	}

	for i := range n {
		queue <- i
	}

	close(queue)
	wg.Wait()
}

// writeReport writes the gremlins JSON a ledger row and the mutation-sweep skill read, plus the killers, which gremlins never said.
func writeReport(path, dir string, mutants []mutant, elapsed time.Duration) error {
	type mutation struct {
		Type    string   `json:"type"`
		Status  string   `json:"status"`
		Line    int      `json:"line"`
		Column  int      `json:"column"`
		Killers []string `json:"killed_by,omitempty"`
	}

	type file struct {
		Name      string     `json:"file_name"`
		Mutations []mutation `json:"mutations"`
	}

	byFile := map[string][]mutation{}
	counts := map[string]int{}

	for _, m := range mutants {
		name := strings.TrimPrefix(m.File, strings.TrimPrefix(dir, "./")+"/")
		byFile[name] = append(byFile[name], mutation{m.Type, m.Outcome, m.Line, m.Column, m.Killers})
		counts[m.Outcome]++
	}

	files := make([]file, 0, len(byFile))
	for name, mutations := range byFile {
		files = append(files, file{name, mutations})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	killed, lived, notCovered := counts["KILLED"], counts["LIVED"], counts["NOT COVERED"]

	var efficacy, coverage float64
	if killed+lived > 0 {
		efficacy = float64(killed) / float64(killed+lived) * 100                  //nolint:mnd // a percentage
		coverage = float64(killed+lived) / float64(killed+lived+notCovered) * 100 //nolint:mnd // a percentage
	}

	encoded, err := json.Marshal(map[string]any{
		"go_module": "github.com/jtarchie/steps", "files": files,
		"test_efficacy": efficacy, "mutations_coverage": coverage, "mutants_total": len(mutants),
		"mutants_killed": killed, "mutants_lived": lived, "mutants_not_viable": counts["NOT VIABLE"],
		"mutants_not_covered": notCovered, "elapsed_time": elapsed.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = os.MkdirAll(filepath.Dir(path), 0o750) //nolint:gosec // .gremlins/ plus a name made from a package directory
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = os.WriteFile(path, encoded, 0o600) //nolint:gosec // .gremlins/ plus a name made from a package directory
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	fmt.Printf("%s: %d killed, %d lived, %d timed out, %d not covered, %d not viable — efficacy %.2f%%\n", dir, killed, lived, counts["TIMED OUT"], notCovered, counts["NOT VIABLE"], efficacy)

	return nil
}

// sweepArgs: a package, then optionally the workers and a rev to scope to.
func sweepArgs(args []string) (string, int, string, error) {
	if len(args) == 0 {
		return "", 0, "", errors.New("sweep needs a package directory")
	}

	workers := sweepWorkers()

	if len(args) > 1 && args[1] != "" {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return "", 0, "", fmt.Errorf("workers %q: %w", args[1], err)
		}

		workers = n
	}

	since := ""
	if len(args) > 2 { //nolint:mnd // package, workers, rev
		since = args[2]
	}

	return args[0], workers, since, nil
}

// sweepWorkers is half the cores: each worker is a go build or a test binary, and a loaded machine times mutants out.
func sweepWorkers() int {
	return max(runtime.NumCPU()/2, 1) //nolint:mnd // half
}
