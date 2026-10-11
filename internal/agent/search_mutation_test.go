package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func searchWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSearchHeadLimitZeroMeansTheDefault(t *testing.T) {
	t.Parallel()

	opts, errResult := buildSearchOpts(map[string]any{"head_limit": float64(0)}, "x", "", "files_with_matches")
	if errResult != nil {
		t.Fatalf("unexpected error: %v", errResult)
	}

	if opts.headLimit != defaultSearchPathResults {
		t.Errorf("headLimit = %d, want the default %d", opts.headLimit, defaultSearchPathResults)
	}
}

func TestSearchStopsAtTheFileScanBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range maxSearchFilesScanned + 1 {
		searchWriteFile(t, dir, fmt.Sprintf("f%05d.txt", i), "")
	}

	result := execSearchFiles(context.Background(), map[string]any{"glob": "*.txt"}, testEnv(dir))

	if result["files_scanned"] != maxSearchFilesScanned {
		t.Errorf("files_scanned = %v, want it stopped at %d", result["files_scanned"], maxSearchFilesScanned)
	}

	if msg, _ := result["message"].(string); !strings.Contains(msg, "stopped after scanning") {
		t.Errorf("message = %q, want the scan budget named", msg)
	}
}

func TestSearchScansAFileOfExactlyTheSizeLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	searchWriteFile(t, dir, "big.txt", "needle\n"+strings.Repeat("x", maxSearchFileBytes-len("needle\n")))

	result := execSearchFiles(context.Background(), map[string]any{"pattern": "needle"}, testEnv(dir))
	if files := searchResultFiles(t, result); len(files) != 1 {
		t.Errorf("files = %v, want big.txt", files)
	}
}

func TestSearchSkipsAFileThatStartsWithNUL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	searchWriteFile(t, dir, "blob.bin", "\x00needle\n")

	result := execSearchFiles(context.Background(), map[string]any{"pattern": "needle"}, testEnv(dir))
	if files := searchResultFiles(t, result); len(files) != 0 {
		t.Errorf("files = %v, want the binary skipped", files)
	}
}

func TestSearchCountAndContentRespectHeadLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	searchWriteFile(t, dir, "a.txt", "needle\nneedle\n")
	searchWriteFile(t, dir, "b.txt", "needle\nneedle\n")

	t.Run("count", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "needle", "output_mode": "count", "head_limit": float64(1),
		}, testEnv(dir))

		counts, _ := result["counts"].([]map[string]any)
		if len(counts) != 1 || result["total"] != 2 {
			t.Errorf("counts = %v, total = %v; want 1 kept of 2 files", counts, result["total"])
		}
	})

	t.Run("content", func(t *testing.T) {
		t.Parallel()

		result := execSearchFiles(context.Background(), map[string]any{
			"pattern": "needle", "output_mode": "content", "head_limit": float64(2),
		}, testEnv(dir))

		matches, _ := result["matches"].([]map[string]any)
		if len(matches) != 2 || result["total"] != 4 {
			t.Errorf("matches = %d, total = %v; want 2 kept of 4", len(matches), result["total"])
		}
	})
}

func TestSearchCompleteResultIsNotTruncated(t *testing.T) {
	t.Parallel()

	dir := searchFixture(t)

	result := execSearchFiles(context.Background(), map[string]any{"pattern": "RunJob"}, testEnv(dir))
	if result["truncated"] != false || result["message"] != nil {
		t.Errorf("truncated = %v, message = %v; want a complete result reported as such", result["truncated"], result["message"])
	}
}

func TestSearchContentBudgetExactlyFilled(t *testing.T) {
	t.Parallel()

	const path = "a.txt"

	perMatch := maxSearchContentBudgetBytes / 56
	line := strings.Repeat("x", perMatch-len(path)-searchMatchOverheadBytes)

	matches := make([]searchMatch, 60)
	for i := range matches {
		matches[i] = searchMatch{path: path, line: i + 1, text: line}
	}

	var r searchResult

	r.addMatches(matches, len(matches), maxSearchContentResults)

	if len(r.matches) != 56 || r.contentBytes != maxSearchContentBudgetBytes {
		t.Errorf("kept %d matches (%d bytes), want 56 filling the %d budget exactly", len(r.matches), r.contentBytes, maxSearchContentBudgetBytes)
	}
}

func TestTruncateSearchLineEdges(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("x", maxSearchLineBytes)
	if got := truncateSearchLine(exact); got != exact {
		t.Errorf("a line of exactly %d bytes came back %d bytes", maxSearchLineBytes, len(got))
	}

	// No rune start anywhere: the cut must stop at the line's beginning rather than run past it.
	if got := truncateSearchLine(strings.Repeat("\x80", 600)); got != " …[line truncated]" {
		t.Errorf("truncateSearchLine(continuation bytes) = %q", got)
	}
}
