package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRanges pins the span rendering the reports are read through.
func TestRanges(t *testing.T) {
	t.Parallel()

	got := ranges([]int{12, 3, 4, 5, 40, 41, 7})
	if got != "3-5, 7, 12, 40-41" {
		t.Errorf("ranges = %q, want %q", got, "3-5, 7, 12, 40-41")
	}
}

// TestHunkPattern pins the diff-header parse against the shapes git emits.
func TestHunkPattern(t *testing.T) {
	t.Parallel()

	for header, want := range map[string][2]string{
		"@@ -10,2 +14,3 @@ func x() {": {"14", "3"},
		"@@ -0,0 +1 @@":                {"1", ""},
		"@@ -5 +5 @@":                  {"5", ""},
	} {
		match := hunkPattern.FindStringSubmatch(header)
		if match == nil {
			t.Fatalf("no match for %q", header)
		}

		if match[1] != want[0] || match[2] != want[1] {
			t.Errorf("%q parsed as start=%q count=%q, want %v", header, match[1], match[2], want)
		}
	}
}

func TestAnEmptyChangeSetIsNotReportedAsCovered(t *testing.T) {
	t.Parallel()

	if got := summary("HEAD", 0, true); got != "coverdiff: no changed Go lines vs HEAD" {
		t.Errorf("summary of nothing = %q, want it to say there was nothing to check", got)
	}

	if got := summary("HEAD", 2, true); !strings.Contains(got, "every changed executable line") {
		t.Errorf("summary of a clean change = %q", got)
	}

	if got := summary("HEAD", 2, false); got != "" {
		t.Errorf("summary of a dirty change = %q, want the per-file lines to stand alone", got)
	}
}

func TestFixturesUnderTestdataAreNotReportable(t *testing.T) {
	t.Parallel()

	for file, want := range map[string]bool{
		"internal/a/a.go":                      true,
		"internal/a/a_test.go":                 false,
		"tools/kindswitch/testdata/src/p/p.go": false,
		"testdata/x.go":                        false,
		"internal/store/sqlite/schema.sql":     false,
		"tools/mutants/main.go":                true,
	} {
		if got := reportable(file); got != want {
			t.Errorf("reportable(%q) = %v, want %v", file, got, want)
		}
	}
}

// TestReport pins the three verdicts: unprofiled, missed lines, clean.
func TestReport(t *testing.T) {
	t.Parallel()

	if report("a.go", []int{1, 2}, nil, nil) {
		t.Error("a file with no coverage data reported clean")
	}

	covered := map[int]bool{1: true}
	uncovered := map[int]bool{2: true}

	if report("a.go", []int{2}, covered, uncovered) {
		t.Error("a changed uncovered line reported clean")
	}

	if !report("a.go", []int{1, 3}, covered, uncovered) {
		t.Error("a covered line plus a non-executable line reported dirty")
	}
}

// TestChangedLinesSeesUntrackedFiles pins the gap the first smoke test
// exposed: git diff never shows an untracked file, and a brand-new file is
// exactly the shape an untested layer arrives in — the failure this tool
// exists for landed almost entirely in new files.
func TestChangedLinesSeesUntrackedFiles(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // fixed args this test wrote
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	err := os.WriteFile(filepath.Join(dir, "fresh.go"), []byte("package x\n\nfunc F() {}\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chdir(wd) })

	err = os.Chdir(dir)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := changedLines("HEAD")
	if err != nil {
		t.Fatalf("changedLines: %v", err)
	}

	if len(changed["fresh.go"]) == 0 {
		t.Fatal("an untracked file is invisible — the exact shape an untested layer arrives in")
	}
}
