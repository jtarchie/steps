// Command coverattr says which e2e test is the only one executing which statements: every package's tests are profiled once, then each e2e test in a process of its own, and the two are compared block by block. Advisory input to pruning ./e2e, and to `mutants judge`, which turns "executes" into "notices a change to".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const modulePrefix = "github.com/jtarchie/steps/"

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverattr:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("coverattr", flag.ContinueOnError)
	dir := flags.String("dir", ".coverattr", "where profiles and attribution.json are written")
	parallel := flags.Int("p", 8, "e2e test processes at once")

	err := flags.Parse(args)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	abs, err := filepath.Abs(*dir)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = os.MkdirAll(filepath.Join(abs, "prof"), 0o750)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = profilePackages(ctx, abs)
	if err != nil {
		return err
	}

	seconds, err := profileE2E(ctx, abs, *parallel)
	if err != nil {
		return err
	}

	pkg, tests, err := readProfiles(abs, seconds)
	if err != nil {
		return err
	}

	report := attribute(pkg, tests, seconds)
	report.print(out)

	encoded, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = os.WriteFile(filepath.Join(abs, "attribution.json"), encoded, 0o600)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// profilePackages runs every package's tests but ./e2e's, each counting what it executes anywhere in internal/.
func profilePackages(ctx context.Context, dir string) error {
	list, err := exec.CommandContext(ctx, "go", "list", "./...").Output()
	if err != nil {
		return fmt.Errorf("go list: %w", err)
	}

	pkgs := slices.DeleteFunc(strings.Fields(string(list)), func(pkg string) bool { return pkg == modulePrefix+"e2e" })

	args := append([]string{"test", "-count=1", "-coverpkg=./internal/...", "-coverprofile=" + filepath.Join(dir, "pkg.out")}, pkgs...)

	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // go test over the packages go list named
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("package tests: %w", err)
	}

	return nil
}

// profileE2E runs each top-level e2e test alone in a process, from one binary built with coverage, and answers how long each took.
func profileE2E(ctx context.Context, dir string, parallel int) (map[string]float64, error) {
	binary := filepath.Join(dir, "e2e.test")

	build := exec.CommandContext(ctx, "go", "test", "-c", "-cover", "-coverpkg=./internal/...", "-o", binary, "./e2e") //nolint:gosec // a path under the output directory
	build.Stdout, build.Stderr = os.Stderr, os.Stderr

	err := build.Run()
	if err != nil {
		return nil, fmt.Errorf("building e2e: %w", err)
	}

	listing := exec.CommandContext(ctx, binary, "-test.list", "^Test") //nolint:gosec // the binary just built
	listing.Dir = "e2e"

	out, err := listing.Output()
	if err != nil {
		return nil, fmt.Errorf("listing e2e tests: %w", err)
	}

	names := strings.Fields(string(out))

	var (
		mu       sync.Mutex
		seconds  = map[string]float64{}
		failures []string
		wg       sync.WaitGroup
	)

	slots := make(chan struct{}, max(parallel, 1))

	for _, name := range names {
		wg.Add(1)
		slots <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			start := time.Now()

			//nolint:gosec // a binary this command just built, running a test it listed
			test := exec.CommandContext(ctx, binary, "-test.run", "^"+name+"$", "-test.count=1",
				"-test.coverprofile="+filepath.Join(dir, "prof", name+".out"))
			test.Dir = "e2e"
			output, runErr := test.CombinedOutput()

			mu.Lock()
			defer mu.Unlock()

			seconds[name] = time.Since(start).Seconds()

			if runErr != nil {
				failures = append(failures, name+":\n"+string(output))
			}
		}()
	}

	wg.Wait()

	if len(failures) > 0 {
		// A failing test's profile is what it reached before failing, which is not what it covers.
		return nil, fmt.Errorf("%d e2e tests failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}

	return seconds, nil
}

// readProfiles reads back the package tests' profile and each e2e test's.
func readProfiles(dir string, seconds map[string]float64) (profile, map[string]profile, error) {
	pkg, err := readProfile(filepath.Join(dir, "pkg.out"))
	if err != nil {
		return profile{}, nil, err
	}

	tests := map[string]profile{}

	for name := range seconds {
		tests[name], err = readProfile(filepath.Join(dir, "prof", name+".out"))
		if err != nil {
			return profile{}, nil, err
		}
	}

	return pkg, tests, nil
}

// profile is a cover profile's blocks: each one's statement count, and whether anything executed it.
type profile struct {
	statements map[string]int
	covered    map[string]bool
}

func readProfile(path string) (profile, error) {
	file, err := os.Open(path) //nolint:gosec // a profile this command wrote
	if err != nil {
		return profile{}, fmt.Errorf("%w", err)
	}
	defer func() { _ = file.Close() }()

	return parseProfile(file)
}

// parseProfile reads "file:start,end statements count" lines; a block repeated across packages' profiles is covered if any of them covered it.
func parseProfile(r io.Reader) (profile, error) {
	p := profile{statements: map[string]int{}, covered: map[string]bool{}}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}

		statements, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}

		count, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}

		p.statements[fields[0]] = statements
		if count > 0 {
			p.covered[fields[0]] = true
		}
	}

	err := scanner.Err()
	if err != nil {
		return p, fmt.Errorf("reading a profile: %w", err)
	}

	return p, nil
}

// Block is a span only e2e executes, and the e2e tests that do.
type Block struct {
	Block      string   `json:"block"`
	Statements int      `json:"statements"`
	Tests      []string `json:"tests"`
}

// Test is one e2e test: what only e2e executes that it does, and of that, what no other e2e test does.
type Test struct {
	Name    string         `json:"name"`
	Seconds float64        `json:"seconds"`
	E2EOnly int            `json:"e2e_only"`
	Unique  int            `json:"unique"`
	Where   map[string]int `json:"where,omitempty"`
}

// Report is the attribution, in statements.
type Report struct {
	Statements  int            `json:"statements"`
	PkgCovered  int            `json:"pkg_covered"`
	BothCovered int            `json:"both_covered"`
	E2EOnly     int            `json:"e2e_only"`
	ByPackage   map[string]int `json:"by_package"`
	Blocks      []Block        `json:"blocks"`
	Tests       []Test         `json:"tests"`
}

// attribute compares what the package tests executed with what each e2e test did.
func attribute(pkg profile, tests map[string]profile, seconds map[string]float64) Report {
	statements := map[string]int{}
	for block, n := range pkg.statements {
		statements[block] = n
	}

	holders := map[string][]string{}

	for name, test := range tests {
		for block, n := range test.statements {
			statements[block] = n
		}

		for block := range test.covered {
			holders[block] = append(holders[block], name)
		}
	}

	report := Report{ByPackage: map[string]int{}}

	for block, n := range statements {
		report.Statements += n

		switch {
		case pkg.covered[block]:
			report.PkgCovered += n
			report.BothCovered += n
		case len(holders[block]) > 0:
			report.BothCovered += n
			report.E2EOnly += n
			report.ByPackage[packageOf(block)] += n

			sort.Strings(holders[block])
			report.Blocks = append(report.Blocks, Block{Block: block, Statements: n, Tests: holders[block]})
		}
	}

	sort.Slice(report.Blocks, func(i, j int) bool { return report.Blocks[i].Block < report.Blocks[j].Block })

	report.Tests = testRows(pkg, tests, seconds, statements, holders)

	sort.Slice(report.Tests, func(i, j int) bool {
		if report.Tests[i].Unique != report.Tests[j].Unique {
			return report.Tests[i].Unique > report.Tests[j].Unique
		}

		return report.Tests[i].Name < report.Tests[j].Name
	})

	return report
}

// testRows is each e2e test's share: what only e2e executes that it does, and of that, what no other e2e test does.
func testRows(pkg profile, tests map[string]profile, seconds map[string]float64, statements map[string]int, holders map[string][]string) []Test {
	rows := make([]Test, 0, len(tests))

	for name, test := range tests {
		row := Test{Name: name, Seconds: seconds[name], Where: map[string]int{}}

		for block := range test.covered {
			if pkg.covered[block] {
				continue
			}

			row.E2EOnly += statements[block]

			if len(holders[block]) == 1 {
				row.Unique += statements[block]
				row.Where[packageOf(block)] += statements[block]
			}
		}

		rows = append(rows, row)
	}

	return rows
}

func packageOf(block string) string {
	file, _, _ := strings.Cut(block, ":")

	return filepath.Dir(strings.TrimPrefix(file, modulePrefix))
}

func (r Report) print(out io.Writer) {
	percent := func(n int) float64 { return 100 * float64(n) / float64(max(r.Statements, 1)) }

	_, _ = fmt.Fprintf(out, "%d statements in internal/: package tests execute %.1f%%, with e2e %.1f%%; %d only under e2e\n",
		r.Statements, percent(r.PkgCovered), percent(r.BothCovered), r.E2EOnly)

	packages := make([]string, 0, len(r.ByPackage))
	for pkg := range r.ByPackage {
		packages = append(packages, pkg)
	}

	sort.Slice(packages, func(i, j int) bool { return r.ByPackage[packages[i]] > r.ByPackage[packages[j]] })

	for _, pkg := range packages {
		_, _ = fmt.Fprintf(out, "%7d %s\n", r.ByPackage[pkg], pkg)
	}

	var none, noneSeconds, total float64

	for _, test := range r.Tests {
		total += test.Seconds

		if test.Unique == 0 {
			none++
			noneSeconds += test.Seconds
		}
	}

	_, _ = fmt.Fprintf(out, "\n%.0f of %d e2e tests execute nothing no other test does (%.0fs of %.0fs). Executing is not asserting: `mutants judge` says which of them notice a change.\n",
		none, len(r.Tests), noneSeconds, total)

	for _, test := range r.Tests {
		if test.Unique == 0 {
			break
		}

		_, _ = fmt.Fprintf(out, "%6d %6d %5.1fs %s\n", test.Unique, test.E2EOnly, test.Seconds, test.Name)
	}
}
