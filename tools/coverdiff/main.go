// Command coverdiff reports changed lines the tests never executed.
//
// It exists because of a measured failure, not a metric: an entire layer of
// eviction handling landed across several commits with zero coverage, and a
// later audit proved that deleting any of it shipped green through the full
// validation sequence. Overall coverage percentages would not have said so —
// the repo's number barely moved — but "which of the lines I just changed
// did no test run" would have, at commit time.
//
// Advisory by design: it prints and exits zero. Some changed lines are
// legitimately uncoverable here (CLI wiring, error branches that need a cloud
// account), so a hard gate would grow a threshold-and-exclusion system that
// is its own maintenance. The contract is CLAUDE.md's: run it before
// committing, read what it prints, and either add the test or know why not.
//
// Usage:
//
//	go test ./... -coverprofile=.cover.out
//	go run ./tools/coverdiff -profile .cover.out -base HEAD
//
// -base is any git rev; HEAD means "the working tree's uncommitted changes",
// HEAD~1 reviews the last commit.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/cover"
)

func main() {
	profilePath := flag.String("profile", ".cover.out", "coverage profile from go test -coverprofile")
	base := flag.String("base", "HEAD", "git rev to diff against")
	flag.Parse()

	err := run(*profilePath, *base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "coverdiff: %v\n", err)
		os.Exit(1)
	}
}

func run(profilePath, base string) error {
	covered, uncovered, err := lineSets(profilePath)
	if err != nil {
		return err
	}

	changed, err := changedLines(base)
	if err != nil {
		return err
	}

	files := make([]string, 0, len(changed))
	for file := range changed {
		files = append(files, file)
	}

	sort.Strings(files)

	clean := true

	for _, file := range files {
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}

		if !report(file, changed[file], covered[file], uncovered[file]) {
			clean = false
		}
	}

	if clean {
		fmt.Printf("coverdiff: every changed executable line vs %s was executed by a test\n", base)
	}

	return nil
}

// lineSets reads the profile into per-file sets: lines inside an executed
// block, and lines inside a block that never ran.
func lineSets(profilePath string) (covered, uncovered map[string]map[int]bool, err error) {
	profiles, err := cover.ParseProfiles(profilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", profilePath, err)
	}

	module, err := moduleName()
	if err != nil {
		return nil, nil, err
	}

	covered, uncovered = map[string]map[int]bool{}, map[string]map[int]bool{}

	for _, profile := range profiles {
		name := strings.TrimPrefix(profile.FileName, module+"/")

		for _, block := range profile.Blocks {
			for line := block.StartLine; line <= block.EndLine; line++ {
				if block.Count > 0 {
					mark(covered, name, line)
				} else {
					mark(uncovered, name, line)
				}
			}
		}
	}

	return covered, uncovered, nil
}

// report prints one file's verdict, answering whether it was clean.
func report(file string, changed []int, covered, uncovered map[int]bool) bool {
	if len(covered) == 0 && len(uncovered) == 0 {
		fmt.Printf("%s: no coverage data — the package has no tests exercising it\n", file)

		return false
	}

	var missed []int

	for _, line := range changed {
		// A line in no block at all is a declaration, brace or comment: not
		// executable, not reportable.
		if uncovered[line] && !covered[line] {
			missed = append(missed, line)
		}
	}

	if len(missed) > 0 {
		fmt.Printf("%s: changed lines never executed by a test: %s\n", file, ranges(missed))

		return false
	}

	return true
}

func mark(set map[string]map[int]bool, file string, line int) {
	if set[file] == nil {
		set[file] = map[int]bool{}
	}

	set[file][line] = true
}

func moduleName() (string, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m").Output()
	if err != nil {
		return "", fmt.Errorf("go list -m: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// hunkPattern reads the new-file side of a @@ header: start line and count.
var hunkPattern = regexp.MustCompile(`^@@ -[0-9,]+ \+([0-9]+)(?:,([0-9]+))? @@`)

// changedLines maps each file touched since base to the line numbers its
// diff added or modified.
func changedLines(base string) (map[string][]int, error) {
	out, err := exec.CommandContext(context.Background(), "git", "diff", "-U0", base, "--", "*.go").Output() //nolint:gosec // base is the operator's own rev argument
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", base, err)
	}

	changed := map[string][]int{}

	// Untracked files first: git diff never shows them, and a brand-new
	// file is exactly the shape an untested layer arrives in — the failure
	// this tool exists for landed almost entirely in new files. Every line
	// of one counts as changed.
	err = addUntracked(changed)
	if err != nil {
		return nil, err
	}

	file := ""

	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if name, ok := strings.CutPrefix(line, "+++ b/"); ok {
			file = name

			continue
		}

		match := hunkPattern.FindStringSubmatch(line)
		if match == nil || file == "" {
			continue
		}

		start, _ := strconv.Atoi(match[1])

		count := 1
		if match[2] != "" {
			count, _ = strconv.Atoi(match[2])
		}

		for i := range count {
			changed[file] = append(changed[file], start+i)
		}
	}

	err = scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("reading the diff: %w", err)
	}

	return changed, nil
}

// addUntracked marks every line of every untracked .go file as changed.
func addUntracked(changed map[string][]int) error {
	out, err := exec.CommandContext(context.Background(), "git", "ls-files", "--others", "--exclude-standard", "--", "*.go").Output()
	if err != nil {
		return fmt.Errorf("git ls-files: %w", err)
	}

	for _, name := range strings.Fields(strings.TrimSpace(string(out))) {
		data, readErr := os.ReadFile(name) //nolint:gosec // a path git itself listed
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", name, readErr)
		}

		total := bytes.Count(data, []byte("\n")) + 1
		for line := 1; line <= total; line++ {
			changed[name] = append(changed[name], line)
		}
	}

	return nil
}

// ranges renders sorted line numbers as compact spans: 3-7, 12, 40-41.
func ranges(lines []int) string {
	sort.Ints(lines)

	var spans []string

	for i := 0; i < len(lines); {
		j := i

		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}

		if i == j {
			spans = append(spans, strconv.Itoa(lines[i]))
		} else {
			spans = append(spans, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		}

		i = j + 1
	}

	return strings.Join(spans, ", ")
}
