package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchFixture builds a small tree to search: two Go files (one a test),
// a markdown file, a binary file, and a .git directory that must be pruned.
func searchFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	files := map[string]string{
		"main.go":               "package main\n\nfunc RunJob() {}\n\nfunc helper() {}\n",
		"internal/run.go":       "package internal\n\n// RunJob is called here\nfunc callRunJob() { RunJob() }\n",
		"internal/run_test.go":  "package internal\n\nfunc TestRunJob(t *testing.T) {}\n",
		"docs/guide.md":         "# Guide\n\nRunJob is the entrypoint.\n",
		".git/objects/deadbeef": "RunJob should never be found here\n",
	}

	for name, content := range files {
		path := filepath.Join(dir, name)

		err := os.MkdirAll(filepath.Dir(path), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	// A binary file: a NUL byte in the head must make it skipped even though
	// the pattern would otherwise match.
	err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("RunJob\x00\x01\x02"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// searchResultFiles asserts a successful search and returns its file list.
func searchResultFiles(t *testing.T, result map[string]any) []string {
	t.Helper()

	if errMsg, bad := result["error"]; bad {
		t.Fatalf("unexpected error: %v", errMsg)
	}

	files, ok := result["files"].([]string)
	if !ok {
		t.Fatalf("files = %#v, want []string", result["files"])
	}

	return files
}

func TestExecSearchFiles(t *testing.T) {
	t.Parallel()

	dir := searchFixture(t)

	t.Run("pattern lists matching files and prunes .git", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{"pattern": "RunJob"}, testEnv(dir))
		files := searchResultFiles(t, result)

		joined := strings.Join(files, ",")
		for _, want := range []string{"main.go", "internal/run.go", "docs/guide.md"} {
			if !strings.Contains(joined, want) {
				t.Errorf("files = %v, want it to include %q", files, want)
			}
		}

		if strings.Contains(joined, ".git") {
			t.Errorf("files = %v, want .git pruned from the walk", files)
		}

		if strings.Contains(joined, "blob.bin") {
			t.Errorf("files = %v, want the binary file skipped", files)
		}
	})

	t.Run("glob alone is a filename search", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{"glob": "*_test.go"}, testEnv(dir))
		files := searchResultFiles(t, result)

		if len(files) != 1 || !strings.Contains(files[0], "run_test.go") {
			t.Errorf("files = %v, want just internal/run_test.go", files)
		}
	})

	t.Run("pattern and glob together", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{"pattern": "RunJob", "glob": "**/*.md"}, testEnv(dir))
		files := searchResultFiles(t, result)

		if len(files) != 1 || !strings.Contains(files[0], "guide.md") {
			t.Errorf("files = %v, want just docs/guide.md", files)
		}
	})
}

// TestExecSearchFilesOutputModes covers the three result shapes, split from
// TestExecSearchFiles to stay under the linter's per-function complexity cap.
func TestExecSearchFilesOutputModes(t *testing.T) {
	t.Parallel()

	dir := searchFixture(t)

	t.Run("content mode returns line numbers for citation", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "func RunJob", "output_mode": "content",
		}, testEnv(dir))

		matches, ok := result["matches"].([]map[string]any)
		if !ok || len(matches) != 1 {
			t.Fatalf("matches = %#v, want exactly one", result["matches"])
		}

		if got := matches[0]["line"]; got != 3 {
			t.Errorf("line = %v, want 3", got)
		}

		if got := matches[0]["path"]; got != "main.go" {
			t.Errorf("path = %v, want main.go", got)
		}
	})

	t.Run("count mode reports matches per file", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "RunJob", "output_mode": "count",
		}, testEnv(dir))

		counts, ok := result["counts"].([]map[string]any)
		if !ok || len(counts) == 0 {
			t.Fatalf("counts = %#v, want per-file counts", result["counts"])
		}
	})

	t.Run("case_insensitive matches regardless of case", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "runjob", "case_insensitive": true,
		}, testEnv(dir))

		if files := searchResultFiles(t, result); len(files) == 0 {
			t.Error("case_insensitive search found nothing")
		}
	})
}

// TestExecSearchFilesCaps proves the bound is arithmetic rather than a
// post-hoc truncation: head_limit is clamped to the ceiling, total reports
// the true scale, and a long line is cut with a marker.
func TestExecSearchFilesCaps(t *testing.T) {
	t.Parallel()

	t.Run("head_limit caps results while total reports the truth", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		for i := range 10 {
			err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))+".txt"), []byte("needle\n"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
		}

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "needle", "head_limit": 3,
		}, testEnv(dir))

		files := searchResultFiles(t, result)
		if len(files) != 3 {
			t.Errorf("files = %d, want 3 (head_limit)", len(files))
		}

		if got := result["total"]; got != 10 {
			t.Errorf("total = %v, want 10 — the true count, not what was kept", got)
		}

		if got := result["truncated"]; got != true {
			t.Errorf("truncated = %v, want true", got)
		}

		if result["message"] == nil {
			t.Error("want a message telling the model to narrow rather than page")
		}
	})

	t.Run("head_limit above the ceiling is clamped", func(t *testing.T) {
		t.Parallel()

		opts, errResult := buildSearchOpts(map[string]any{"head_limit": 9999}, "x", "", "content")
		if errResult != nil {
			t.Fatalf("unexpected error: %v", errResult)
		}

		if opts.headLimit != maxSearchContentResults {
			t.Errorf("headLimit = %d, want it clamped to %d", opts.headLimit, maxSearchContentResults)
		}
	})
}

// TestExecSearchFilesTruncatesLongLines keeps one pathological line from
// consuming the whole result budget.
func TestExecSearchFilesTruncatesLongLines(t *testing.T) {
	t.Parallel()

	t.Run("a long matching line is truncated with a marker", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		long := "needle" + strings.Repeat("x", maxSearchLineBytes*2)
		err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(long+"\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "needle", "output_mode": "content",
		}, testEnv(dir))

		matches, ok := result["matches"].([]map[string]any)
		if !ok || len(matches) != 1 {
			t.Fatalf("matches = %#v, want one", result["matches"])
		}

		text, _ := matches[0]["text"].(string)
		if len(text) > maxSearchLineBytes+len(" …[line truncated]") {
			t.Errorf("text is %d bytes, want it capped near %d", len(text), maxSearchLineBytes)
		}

		if !strings.Contains(text, "line truncated") {
			t.Error("want a marker so the model does not copy a half-line as verbatim text")
		}
	})
}

func TestExecSearchFilesErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"neither pattern nor glob", map[string]any{}, "at least one"},
		{"invalid regexp", map[string]any{"pattern": "([unclosed"}, "invalid pattern"},
		{"unknown output_mode", map[string]any{"pattern": "x", "output_mode": "nope"}, "unknown output_mode"},
		{"content mode without a pattern", map[string]any{"glob": "*.go", "output_mode": "content"}, "needs a pattern"},
		{"** not at the start", map[string]any{"glob": "src/**/*.go"}, "only supported as a leading segment"},
		{"path escaping the workspace", map[string]any{"pattern": "x", "path": "../.."}, "escapes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := execSearchFiles(context.Background(), tc.args, testEnv(dir))

			errMsg, ok := result["error"].(string)
			if !ok {
				t.Fatalf("expected an error, got %#v", result)
			}

			if !strings.Contains(errMsg, tc.want) {
				t.Errorf("error = %q, want it to mention %q", errMsg, tc.want)
			}
		})
	}
}

// TestSearchWorstCaseFitsInlineBudget is the load-bearing test for this
// tool's whole reason to exist: a fully saturated content-mode result must
// still be small enough that it can never do what an uncapped fuzzy search
// did to a real run's context window.
//
// The bound is the content BYTE budget, not the result count: addMatches
// charges every kept match its line text PLUS its path plus a fixed
// scaffolding allowance, and stops before the budget is exceeded — so kept
// bytes never exceed the budget, regardless of how the line-length and
// result-count ceilings are tuned, or how deep the tree being searched is.
// (Charging the path is what makes that true: 200 short matches under long
// generated paths would otherwise be nearly free here and enormous on the
// wire.) The margin below the cap covers the result envelope around the
// matches.
func TestSearchWorstCaseFitsInlineBudget(t *testing.T) {
	t.Parallel()

	worst := maxSearchContentBudgetBytes + maxSearchLineBytes + searchMatchOverheadBytes
	if worst >= maxToolOutputBytes {
		t.Errorf("worst-case content result is ~%d bytes, at or over the %d inline cap; the bound must hold by arithmetic, not by truncation", worst, maxToolOutputBytes)
	}
}

// TestSearchContentByteBudgetStopsKeeping drives addMatches past the byte
// budget and checks it stops keeping matches while total still counts them.
func TestSearchContentByteBudgetStopsKeeping(t *testing.T) {
	t.Parallel()

	var r searchResult

	const path = "big.txt"

	line := strings.Repeat("x", maxSearchLineBytes)
	perMatch := maxSearchLineBytes + len(path) + searchMatchOverheadBytes
	fits := maxSearchContentBudgetBytes / perMatch

	matches := make([]searchMatch, fits+10)
	for i := range matches {
		matches[i] = searchMatch{path: path, line: i + 1, text: line}
	}

	r.addMatches(matches, len(matches), maxSearchContentResults)

	if len(r.matches) != fits {
		t.Errorf("kept %d matches, want %d — the byte budget must stop collection", len(r.matches), fits)
	}

	if r.total != len(matches) {
		t.Errorf("total = %d, want %d — total reports the true scale even past the budget", r.total, len(matches))
	}

	if r.contentBytes > maxSearchContentBudgetBytes {
		t.Errorf("contentBytes = %d, over the %d budget", r.contentBytes, maxSearchContentBudgetBytes)
	}
}

func TestMatchGlob(t *testing.T) {
	t.Parallel()

	cases := []struct {
		rel, glob string
		want      bool
	}{
		{"main.go", "*.go", true},
		{"internal/agent/tools.go", "*.go", true},    // bare glob matches the base name
		{"internal/agent/tools.go", "**/*.go", true}, // leading ** is any depth
		{"internal/agent/tools_test.go", "*_test.go", true},
		{"internal/agent/tools.go", "*_test.go", false},
		{"docs/guide.md", "*.go", false},
		{"internal/agent/tools.go", "internal/*", false}, // * does not cross a separator
	}

	for _, tc := range cases {
		if got := matchGlob(tc.rel, tc.glob); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.rel, tc.glob, got, tc.want)
		}
	}
}
