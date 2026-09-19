// Command mutants is the bookkeeping around a gremlins sweep: which package is stalest, and which files a scoped run may skip. See .claude/skills/mutation-sweep.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ledgerPath = "tools/mutation-ledger.json"

type ledger struct {
	Packages map[string]row `json:"packages"`
}

type row struct {
	SweptAt    string       `json:"swept_at"`
	Date       string       `json:"date"`
	Killed     int          `json:"killed"`
	Lived      int          `json:"lived"`
	TimedOut   int          `json:"timed_out"`
	NotCovered int          `json:"not_covered"`
	Efficacy   float64      `json:"efficacy"`
	Note       string       `json:"note"`
	Equivalent []equivalent `json:"equivalent"`
}

type equivalent struct {
	Mutant string `json:"mutant"`
	Why    string `json:"why"`
}

func main() {
	err := run(context.Background(), os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mutants: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	usage := errors.New("usage: mutants stale | summary | all [package dir...] | excludes <package dir> <since rev, or empty> | record <package dir> <gremlins json>")

	if len(args) == 0 {
		return usage
	}

	verbs := map[string]struct {
		operands int
		run      func(operands []string) error
	}{
		"stale":    {0, func([]string) error { return stale(ctx) }},
		"summary":  {0, func([]string) error { return printSummary(ctx) }},
		"all":      {-1, func(only []string) error { return sweepAll(ctx, only) }},
		"excludes": {2, func(o []string) error { return excludes(ctx, o[0], o[1]) }},   //nolint:mnd // a package and a rev
		"record":   {2, func(o []string) error { return recordFile(ctx, o[0], o[1]) }}, //nolint:mnd // a package and a report
	}

	verb, known := verbs[args[0]]
	if !known || (verb.operands >= 0 && len(args)-1 != verb.operands) {
		return usage
	}

	return verb.run(args[1:])
}

type staleness struct {
	pkg     string
	swept   string
	changed int
	never   bool
}

func stale(ctx context.Context) error {
	rows, err := stalenessOfAll(ctx)
	if err != nil {
		return err
	}

	for _, r := range rows {
		if r.never {
			fmt.Printf("%6d lines  never swept        %s\n", r.changed, r.pkg)

			continue
		}

		fmt.Printf("%6d changed  since %s  %s\n", r.changed, r.swept, r.pkg)
	}

	return nil
}

func stalenessOfAll(ctx context.Context) ([]staleness, error) {
	book, err := readLedger()
	if err != nil {
		return nil, err
	}

	dirs, err := packageDirs(ctx)
	if err != nil {
		return nil, err
	}

	rows := make([]staleness, 0, len(dirs))

	for _, dir := range dirs {
		entry, swept := book.Packages[dir]
		if !swept {
			lines, err := sourceLines(dir)
			if err != nil {
				return nil, err
			}

			rows = append(rows, staleness{pkg: dir, changed: lines, never: true})

			continue
		}

		changed, err := linesChangedSince(ctx, dir, entry.SweptAt)
		if err != nil {
			return nil, err
		}

		rows = append(rows, staleness{pkg: dir, swept: entry.SweptAt + " " + entry.Date, changed: changed})
	}

	rank(rows)

	return rows, nil
}

// printSummary is the line `task` ends on. There is no CI and no scheduler here, so nothing else would ever say that a sweep is due.
func printSummary(ctx context.Context) error {
	rows, err := stalenessOfAll(ctx)
	if err != nil {
		return err
	}

	if line := summary(rows); line != "" {
		fmt.Println(line)
	}

	return nil
}

// driftWorthSaying is how many changed lines make a swept package worth naming: below it, a sweep would mostly re-kill the same mutants.
const driftWorthSaying = 300

// summary is one line, or nothing at all when nothing is due — a nag that speaks on every green run is one nobody reads by the second week.
func summary(rows []staleness) string {
	var (
		never   int
		drifted []string
	)

	for _, r := range rows {
		switch {
		case r.never:
			never++
		case r.changed >= driftWorthSaying:
			drifted = append(drifted, fmt.Sprintf("%s (%d lines since %s)", r.pkg, r.changed, r.swept))
		}
	}

	if never == 0 && len(drifted) == 0 {
		return ""
	}

	var parts []string

	if never > 0 {
		parts = append(parts, fmt.Sprintf("%d packages never swept", never))
	}

	if len(drifted) > 0 {
		parts = append(parts, "drifted: "+strings.Join(drifted, ", "))
	}

	return "mutation testing: " + strings.Join(parts, "; ") + " — `task mutate-stale` ranks them, the mutation-sweep skill sweeps one, `task mutate-all` measures everything overnight"
}

// rank puts never-swept packages first and the most churn first within each group: an unswept package has unknown efficacy, which is worse than a known one that has drifted.
func rank(rows []staleness) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].never != rows[j].never {
			return rows[i].never
		}

		return rows[i].changed > rows[j].changed
	})
}

func excludes(ctx context.Context, dir, since string) error {
	if since == "" {
		return nil
	}

	all, err := sourceFiles(dir)
	if err != nil {
		return err
	}

	out, err := git(ctx, "diff", "--name-only", since, "--", dir)
	if err != nil {
		return err
	}

	changed := map[string]bool{}
	for _, name := range strings.Fields(out) {
		changed[name] = true
	}

	fmt.Println(strings.Join(excludeFlags(dir, all, changed), " "))

	return nil
}

// excludeFlags stands in for gremlins' --diff, which is broken in package mode: it compares repo-relative diff paths against package-relative mutant paths and so skips every mutant. The patterns are package-relative for the same reason.
func excludeFlags(dir string, all []string, changed map[string]bool) []string {
	var flags []string

	for _, file := range all {
		if changed[file] {
			continue
		}

		rel, err := filepath.Rel(dir, file)
		if err != nil {
			rel = file
		}

		// Unquoted: the Taskfile splices this in with $(...), and a shell does not re-parse quotes that arrive by substitution — they would become part of the pattern.
		flags = append(flags, "-E", "(^|/)"+regexp.QuoteMeta(filepath.ToSlash(rel))+"$")
	}

	return flags
}

func readLedger() (ledger, error) {
	book := ledger{Packages: map[string]row{}}

	raw, err := os.ReadFile(ledgerPath)
	if errors.Is(err, fs.ErrNotExist) {
		return book, nil
	}

	if err != nil {
		return book, fmt.Errorf("reading the ledger: %w", err)
	}

	err = json.Unmarshal(raw, &book)
	if err != nil {
		return book, fmt.Errorf("%s: %w", ledgerPath, err)
	}

	return book, nil
}

func packageDirs(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-f", `{{if .GoFiles}}{{.Dir}}{{end}}`, "./...")

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}

	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}

	var dirs []string

	for _, dir := range strings.Fields(string(out)) {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." {
			continue
		}

		dirs = append(dirs, "./"+filepath.ToSlash(rel))
	}

	return dirs, nil
}

// sourceFiles is every non-test Go file under dir, recursively, because gremlins' package mode walks subdirectories too.
func sourceFiles(dir string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error { //nolint:gosec // dir is a package directory of this repository, named on the command line by whoever runs the sweep
		if err != nil {
			return err
		}

		if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, filepath.ToSlash(filepath.Clean(path)))
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dir, err)
	}

	return files, nil
}

func sourceLines(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", dir, err)
	}

	total := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a Go source file of this repository
		if err != nil {
			return 0, fmt.Errorf("reading %s: %w", name, err)
		}

		total += strings.Count(string(raw), "\n")
	}

	return total, nil
}

func linesChangedSince(ctx context.Context, dir, since string) (int, error) {
	out, err := git(ctx, "diff", "--numstat", since, "--", dir)
	if err != nil {
		return 0, err
	}

	return sumNumstat(out, dir), nil
}

// sumNumstat counts added plus deleted lines in the package's own non-test files; a subdirectory is its own package with its own row.
func sumNumstat(numstat, dir string) int {
	total := 0

	for _, line := range strings.Split(numstat, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || strings.HasSuffix(fields[2], "_test.go") || !strings.HasSuffix(fields[2], ".go") {
			continue
		}

		if filepath.ToSlash(filepath.Dir(fields[2])) != strings.TrimPrefix(dir, "./") {
			continue
		}

		added, _ := strconv.Atoi(fields[0])
		deleted, _ := strconv.Atoi(fields[1])
		total += added + deleted
	}

	return total
}

func git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed subcommands; the variable arguments are a rev from the ledger and a package directory from go list

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	return string(out), nil
}

// gremlinsResult is the part of `gremlins unleash -o` a ledger row is made from.
type gremlinsResult struct {
	Efficacy   float64 `json:"test_efficacy"`
	Killed     int     `json:"mutants_killed"`
	Lived      int     `json:"mutants_lived"`
	NotCovered int     `json:"mutants_not_covered"`
	Files      []struct {
		Mutations []struct {
			Status string `json:"status"`
		} `json:"mutations"`
	} `json:"files"`
}

// record turns one measurement into a ledger row. What triage wrote — the note and the equivalents — is kept, because a re-measurement did not make those decisions and must not unmake them.
func (book *ledger) record(pkg string, report []byte, commit, date string) error {
	var result gremlinsResult

	err := json.Unmarshal(report, &result)
	if err != nil {
		return fmt.Errorf("reading the gremlins report for %s: %w", pkg, err)
	}

	before, swept := book.Packages[pkg]

	efficacy := float64(int(result.Efficacy*100+0.5)) / 100 //nolint:mnd // two decimal places, which is what gremlins prints

	if swept && efficacy < before.Efficacy {
		return fmt.Errorf("%s: efficacy fell from %.2f to %.2f — that is a finding, not a number to overwrite. Triage what now survives; edit the ledger by hand only once the drop is understood", pkg, before.Efficacy, efficacy)
	}

	timedOut := 0

	for _, file := range result.Files {
		for _, mutation := range file.Mutations {
			if mutation.Status == "TIMED OUT" {
				timedOut++
			}
		}
	}

	after := before
	after.SweptAt, after.Date = commit, date
	after.Killed, after.Lived, after.NotCovered, after.TimedOut, after.Efficacy = result.Killed, result.Lived, result.NotCovered, timedOut, efficacy

	if !swept && result.Lived > 0 {
		after.Note = "Measured only. The survivors are NOT triaged: package mode never runs ./e2e, so some will die there, and nobody has looked."
	}

	if after.Equivalent == nil {
		after.Equivalent = []equivalent{}
	}

	book.Packages[pkg] = after

	return nil
}

func recordFile(ctx context.Context, pkg, reportPath string) error {
	report, err := os.ReadFile(reportPath) //nolint:gosec // a results file this tool's own caller just had gremlins write
	if err != nil {
		return fmt.Errorf("reading %s: %w", reportPath, err)
	}

	book, err := readLedger()
	if err != nil {
		return err
	}

	commit, err := git(ctx, "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}

	err = book.record(pkg, report, strings.TrimSpace(commit), time.Now().Format(time.DateOnly))
	if err != nil {
		return err
	}

	return writeLedger(book)
}

func writeLedger(book ledger) error {
	raw, err := json.MarshalIndent(book, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the ledger: %w", err)
	}

	err = os.WriteFile(ledgerPath, append(raw, '\n'), 0o600)
	if err != nil {
		return fmt.Errorf("writing the ledger: %w", err)
	}

	return nil
}

// serialPackages are the ones whose tests bind ports, spawn containers or share the docker daemon: parallel mutants there fail each other's tests and are scored KILLED for the wrong reason. They also take hours, so they go last.
var serialPackages = map[string]bool{ //nolint:gochecknoglobals // a fixed table
	"./internal/pipeline": true, "./internal/venue": true, "./internal/shell": true, "./internal/web": true,
	"./internal/cli": true, "./internal/dockerapi": true, "./internal/shim": true, "./internal/trigger": true,
}

// sweepOrder is cheapest first, serial packages last: a night that gets interrupted should leave the most rows behind, and should not have spent itself inside the one package that takes hours.
func sweepOrder(dirs []string, sizes map[string]int) []string {
	ordered := slices.Clone(dirs)

	sort.SliceStable(ordered, func(i, j int) bool {
		if serialPackages[ordered[i]] != serialPackages[ordered[j]] {
			return serialPackages[ordered[j]]
		}

		return sizes[ordered[i]] < sizes[ordered[j]]
	})

	return ordered
}

// childExcludes keeps a parent package's sweep to its own files. gremlins' package mode walks subdirectories, so ./internal/store alone re-sweeps sqlite and storetest and reports their mutants as its own.
func childExcludes(dir string, all []string) []string {
	var flags []string

	for _, other := range all {
		rel, ok := strings.CutPrefix(other, dir+"/")
		if !ok {
			continue
		}

		child, _, _ := strings.Cut(rel, "/")
		flag := "^" + regexp.QuoteMeta(child) + "/"

		if !slices.Contains(flags, flag) {
			flags = append(flags, "-E", flag)
		}
	}

	return flags
}

// sweepAll MEASURES every package and triages none. Measuring is machine time and can run unattended; triage is judgment and cannot, so the two are kept apart — this fills the ledger with real floors and a survivor count per package, and the mutation-sweep skill works through them afterwards. Resumable: a package already measured at this commit is skipped.
func sweepAll(ctx context.Context, only []string) error {
	dirs, err := packageDirs(ctx)
	if err != nil {
		return err
	}

	// Every package is still needed to know a parent's children; `only` narrows what is swept, not what is known.
	targets := dirs
	if len(only) > 0 {
		targets = only
	}

	sizes := map[string]int{}

	for _, dir := range targets {
		sizes[dir], err = sourceLines(dir)
		if err != nil {
			return err
		}
	}

	head, err := git(ctx, "rev-parse", "--short", "HEAD")
	if err != nil {
		return err
	}

	head = strings.TrimSpace(head)

	var failed []string

	for _, dir := range sweepOrder(targets, sizes) {
		err = measure(ctx, dir, dirs, head, sizes[dir])
		if err != nil {
			// One package failing to measure must not cost the night the rest of them.
			fmt.Fprintf(os.Stderr, "mutants: %v\n", err)

			failed = append(failed, dir)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("not recorded: %s", strings.Join(failed, ", "))
	}

	return nil
}

// measure sweeps one package unless the ledger says it was already measured at this commit, which is what makes an interrupted night resumable.
func measure(ctx context.Context, dir string, all []string, head string, lines int) error {
	book, err := readLedger()
	if err != nil {
		return err
	}

	if book.Packages[dir].SweptAt == head {
		fmt.Printf("== %s: already measured at %s\n", dir, head)

		return nil
	}

	fmt.Printf("== %s (%d lines)\n", dir, lines)

	return sweepOne(ctx, dir, all)
}

func sweepOne(ctx context.Context, dir string, all []string) error {
	// A cached coverage run gives gremlins a one-second baseline, its timeout is that baseline times a coefficient, and TIMED OUT scores as KILLED.
	err := runLoud(ctx, "go", "clean", "-testcache")
	if err != nil {
		return err
	}

	err = os.MkdirAll(".gremlins", 0o750)
	if err != nil {
		return fmt.Errorf("creating .gremlins: %w", err)
	}

	report := filepath.Join(".gremlins", strings.ReplaceAll(strings.TrimPrefix(dir, "./"), "/", "_")+".json")

	workers := "0"
	if serialPackages[dir] {
		workers = "1"
	}

	args := slices.Concat(
		[]string{"tool", "-modfile=go.tool.mod", "gremlins", "unleash", "--timeout-coefficient", "15", "--workers", workers, "-o", report},
		childExcludes(dir, all),
		[]string{dir},
	)

	err = runLoud(ctx, "go", args...)
	if err != nil {
		return err
	}

	return recordFile(ctx, dir, report)
}

func runLoud(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed commands; the variable arguments are package directories from go list
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return nil
}
