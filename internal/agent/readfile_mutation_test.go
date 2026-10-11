package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readfileWrite(t *testing.T, dir, name, content string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadFileAtExactlyTheBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	whole := strings.Repeat("a", maxReadFileBytes)
	readfileWrite(t, dir, "whole.txt", whole)

	t.Run("a full read is returned without a truncation banner", func(t *testing.T) {
		t.Parallel()

		got := execReadFile(context.Background(), map[string]any{"path": "whole.txt"}, testEnv(dir))
		if got["content"] != whole {
			t.Errorf("content is %d bytes starting %.40q, want the file itself", len(got["content"].(string)), got["content"])
		}
	})

	t.Run("a ranged read of one budget-sized line is not truncated", func(t *testing.T) {
		t.Parallel()

		got := execReadFile(context.Background(), map[string]any{"path": "whole.txt", "start_line": float64(1)}, testEnv(dir))
		if got["truncated"] != false || got["content"] != whole {
			t.Errorf("truncated = %v, content %d bytes; want the whole line, untruncated", got["truncated"], len(got["content"].(string)))
		}
	})
}

// The joining newline counts against the budget: two lines totalling exactly maxReadFileBytes fit, one byte more does not.
func TestReadFileRangeBudgetCountsTheNewline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := strings.Repeat("a", 50_000)
	readfileWrite(t, dir, "fits.txt", first+"\n"+strings.Repeat("b", 49_999)+"\n")
	readfileWrite(t, dir, "over.txt", first+"\n"+strings.Repeat("b", 50_000)+"\n")

	fits := execReadFile(context.Background(), map[string]any{"path": "fits.txt", "start_line": float64(1)}, testEnv(dir))
	if fits["end_line"] != 2 || fits["truncated"] != false {
		t.Errorf("fits: end_line = %v, truncated = %v; want both lines", fits["end_line"], fits["truncated"])
	}

	over := execReadFile(context.Background(), map[string]any{"path": "over.txt", "start_line": float64(1)}, testEnv(dir))
	if over["end_line"] != 1 || over["truncated"] != true || over["content"] != first {
		t.Errorf("over: end_line = %v, truncated = %v; want only the first line", over["end_line"], over["truncated"])
	}
}
