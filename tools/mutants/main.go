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
	"sort"
	"strconv"
	"strings"
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
	switch {
	case len(args) == 1 && args[0] == "stale":
		return stale(ctx)
	case len(args) == 3 && args[0] == "excludes":
		return excludes(ctx, args[1], args[2])
	default:
		return errors.New("usage: mutants stale | mutants excludes <package dir> <since rev, or empty>")
	}
}

type staleness struct {
	pkg     string
	swept   string
	changed int
	never   bool
}

func stale(ctx context.Context) error {
	book, err := readLedger()
	if err != nil {
		return err
	}

	dirs, err := packageDirs(ctx)
	if err != nil {
		return err
	}

	rows := make([]staleness, 0, len(dirs))

	for _, dir := range dirs {
		entry, swept := book.Packages[dir]
		if !swept {
			lines, err := sourceLines(dir)
			if err != nil {
				return err
			}

			rows = append(rows, staleness{pkg: dir, changed: lines, never: true})

			continue
		}

		changed, err := linesChangedSince(ctx, dir, entry.SweptAt)
		if err != nil {
			return err
		}

		rows = append(rows, staleness{pkg: dir, swept: entry.SweptAt + " " + entry.Date, changed: changed})
	}

	rank(rows)

	for _, r := range rows {
		if r.never {
			fmt.Printf("%6d lines  never swept        %s\n", r.changed, r.pkg)

			continue
		}

		fmt.Printf("%6d changed  since %s  %s\n", r.changed, r.swept, r.pkg)
	}

	return nil
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
