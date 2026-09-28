// Command testshard runs one test package as several processes of a single compiled test binary, so a package of serial tests finishes in the time of its slowest shard; each process keeps its own globals, which is what t.Setenv, t.Chdir, a swapped os.Stdout and a daemon stopped by SIGINT to its own pid all need, and what t.Parallel cannot give them.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// durationsDir holds each package's per-test wall clock from its last run, which is what balances the next one; gitignored, and a missing file only costs balance.
const durationsDir = ".testshard"

func main() {
	os.Exit(exitCode())
}

func exitCode() int {
	shards := flag.Int("shards", 8, "processes to run each package as")
	race := flag.Bool("race", false, "build the test binary with the race detector")
	timeout := flag.Duration("timeout", 10*time.Minute, "each shard's -test.timeout")
	flag.Parse()

	if flag.NArg() < 1 || *shards < 1 {
		fmt.Fprintln(os.Stderr, "usage: testshard [-shards N] [-race] [-timeout D] <package>...")

		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed bool
	)

	for _, pkg := range flag.Args() {
		wg.Go(func() {
			var out bytes.Buffer

			err := run(ctx, pkg, *shards, *race, *timeout, &out)

			mu.Lock()
			defer mu.Unlock()

			_, _ = os.Stdout.Write(out.Bytes())

			if err != nil {
				fmt.Fprintf(os.Stderr, "testshard: %v\n", err)

				failed = true
			}
		})
	}

	wg.Wait()

	if failed {
		return 1
	}

	return 0
}

func run(ctx context.Context, pkg string, shards int, race bool, timeout time.Duration, out io.Writer) error {
	started := time.Now()

	tmp, err := os.MkdirTemp("", "testshard-")
	if err != nil {
		return err //nolint:wrapcheck // MkdirTemp names the path
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	binary := filepath.Join(tmp, "pkg.test")

	pkgDir, err := build(ctx, pkg, binary, race)
	if err != nil {
		return err
	}

	names, err := listTests(ctx, binary, pkgDir)
	if err != nil {
		return err
	}

	cache := filepath.Join(durationsDir, strings.NewReplacer("/", "_", ".", "_").Replace(pkg)+".json")
	known := loadDurations(cache)

	groups := partition(names, known, shards)
	results := make([]shardResult, len(groups))

	var wg sync.WaitGroup

	for i, group := range groups {
		wg.Go(func() {
			results[i] = runShard(ctx, binary, pkgDir, group, race, timeout)
		})
	}

	wg.Wait()

	var failed []int

	for i, result := range results {
		for name, took := range result.durations {
			known[name] = took
		}

		if result.err != nil {
			failed = append(failed, i)

			_, _ = out.Write(result.output)
			_, _ = fmt.Fprintf(out, "FAIL\tshard %d/%d\t%.1fs\t%d tests: %v\n", i+1, len(groups), result.took.Seconds(), len(groups[i]), result.err)
		}
	}

	saveDurations(cache, known)

	if len(failed) > 0 {
		return fmt.Errorf("%s: %d of %d shards failed", pkg, len(failed), len(groups))
	}

	_, _ = fmt.Fprintf(out, "ok  \t%s\t%.1fs\t%d tests in %d shards\n", pkg, time.Since(started).Seconds(), len(names), len(groups))

	return nil
}

// build compiles pkg's test binary to binary and returns the package's directory, where `go test` would run it.
func build(ctx context.Context, pkg, binary string, race bool) (string, error) {
	args := []string{"test", "-c", "-o", binary}
	if race {
		args = append(args, "-race")
	}

	err := goCommand(ctx, append(args, pkg)...).Run()
	if err != nil {
		return "", fmt.Errorf("building %s: %w", pkg, err)
	}

	dir, err := goCommand(ctx, "list", "-f", "{{.Dir}}", pkg).Output()
	if err != nil {
		return "", fmt.Errorf("locating %s: %w", pkg, err)
	}

	return strings.TrimSpace(string(dir)), nil
}

func goCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // the go tool, on the package this command was asked to run
	cmd.Stderr = os.Stderr

	return cmd
}

// testName is what -test.list prints that -test.run runs; a Benchmark is listed too, and only -test.bench runs it.
var testName = regexp.MustCompile(`^(Test|Fuzz|Example)`)

func listTests(ctx context.Context, binary, dir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, binary, "-test.list", ".")
	cmd.Dir = dir
	cmd.Env = raceEnv()

	listed, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing tests: %w", err)
	}

	var names []string

	for name := range strings.FieldsSeq(string(listed)) {
		if testName.MatchString(name) {
			names = append(names, name)
		}
	}

	return names, nil
}

// raceEnv drops the race runtime's one-second sleep at exit, which exists so a goroutine still running can report; every package here fails its run through goleak if one is still running, so the second buys nothing and costs one per process.
func raceEnv() []string {
	return append(os.Environ(), "GORACE=atexit_sleep_ms=0")
}

// partition deals the longest-known test to the lightest shard first, so shards finish together; a test with no recorded duration counts as the median of those that have one.
func partition(names []string, known map[string]float64, shards int) [][]string {
	guess := 1.0

	if len(known) > 0 {
		all := make([]float64, 0, len(known))
		for _, took := range known {
			all = append(all, took)
		}

		slices.Sort(all)
		guess = all[len(all)/2]
	}

	cost := func(name string) float64 {
		if took, ok := known[name]; ok {
			return took
		}

		return guess
	}

	sorted := slices.Clone(names)
	slices.SortStableFunc(sorted, func(a, b string) int {
		switch ca, cb := cost(a), cost(b); {
		case ca > cb:
			return -1
		case ca < cb:
			return 1
		default:
			return strings.Compare(a, b)
		}
	})

	groups := make([][]string, min(shards, len(sorted)))
	loads := make([]float64, len(groups))

	for _, name := range sorted {
		lightest := 0

		for i := range loads {
			if loads[i] < loads[lightest] {
				lightest = i
			}
		}

		groups[lightest] = append(groups[lightest], name)
		loads[lightest] += cost(name)
	}

	return groups
}

type shardResult struct {
	output    []byte
	durations map[string]float64
	took      time.Duration
	err       error
}

// finished is a top-level test's verdict line under -test.v; a subtest's is indented.
var finished = regexp.MustCompile(`^--- (?:PASS|FAIL|SKIP): (\S+) \(([0-9.]+)s\)`)

func runShard(ctx context.Context, binary, dir string, names []string, race bool, timeout time.Duration) shardResult {
	started := time.Now()

	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = regexp.QuoteMeta(name)
	}

	//nolint:gosec // the binary this command just built, run on names it listed
	cmd := exec.CommandContext(ctx, binary,
		"-test.run", "^("+strings.Join(quoted, "|")+")$",
		"-test.v", "-test.timeout", timeout.String())
	cmd.Dir = dir

	if race {
		cmd.Env = raceEnv()
	}

	var output bytes.Buffer

	cmd.Stdout = &output
	cmd.Stderr = &output

	err := cmd.Run()

	durations := map[string]float64{}

	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	scanner.Buffer(nil, 1<<20)

	for scanner.Scan() {
		match := finished.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}

		took, parseErr := strconv.ParseFloat(match[2], 64)
		if parseErr == nil {
			durations[match[1]] = took
		}
	}

	return shardResult{output: output.Bytes(), durations: durations, took: time.Since(started), err: err}
}

func loadDurations(path string) map[string]float64 {
	known := map[string]float64{}

	data, err := os.ReadFile(path) //nolint:gosec // a path this command derives from the package name, under its own cache dir
	if err != nil {
		return known
	}

	err = json.Unmarshal(data, &known)
	if err != nil {
		return map[string]float64{}
	}

	return known
}

// saveDurations is best-effort: a cache that cannot be written only costs the next run its balance.
func saveDurations(path string, known map[string]float64) {
	data, err := json.MarshalIndent(known, "", "  ")
	if err != nil {
		return
	}

	err = os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return
	}

	_ = os.WriteFile(path, data, 0o600)
}
