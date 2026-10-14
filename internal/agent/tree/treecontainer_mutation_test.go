package tree

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// treecontainerModeRunner records, per script, whether it was run as a read.
type treecontainerModeRunner struct {
	shell.Runner
	readOnly []bool
}

func (r *treecontainerModeRunner) RunCaptureFullLimited(ctx context.Context, _ string, _ int, _ string) (string, string, int, error) {
	r.readOnly = append(r.readOnly, shell.IsReadOnly(ctx))

	return "", "", 0, nil
}

// TestContainerWriteLandsOnlyItsLastScript pins the split writeFile makes: every staging script runs as a read, and only the one that lands the file is a write — the one a placed agent pays a transfer of the step's outputs for.
func TestContainerWriteLandsOnlyItsLastScript(t *testing.T) {
	t.Parallel()

	runner := &treecontainerModeRunner{}
	tree := Container{Runner: runner, dir: "/work", Lost: &Lost{}}

	err := tree.WriteFile(t.Context(), "/work/out.txt", []byte(strings.Repeat("x", 2*maxWriteChunkBytes)), false)
	if err != nil {
		t.Fatal(err)
	}

	if len(runner.readOnly) < 3 {
		t.Fatalf("writeFile ran %d scripts, want a staging write of several", len(runner.readOnly))
	}

	for i, readOnly := range runner.readOnly {
		last := i == len(runner.readOnly)-1
		if readOnly == last {
			t.Errorf("script %d of %d: read-only = %v, want %v", i+1, len(runner.readOnly), readOnly, !last)
		}
	}
}

func TestChunkQuotedSplitsPastTheLimitOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		content string
		chunks  int
	}{
		{strings.Repeat("a", maxWriteChunkBytes), 1},
		{strings.Repeat("a", maxWriteChunkBytes+1), 2},
		{strings.Repeat("a", 2*maxWriteChunkBytes), 2},
		{strings.Repeat("'", maxWriteChunkBytes/4), 1},
		{strings.Repeat("'", maxWriteChunkBytes/4+1), 2},
	}

	for _, tc := range cases {
		got := chunkQuoted(tc.content)
		if len(got) != tc.chunks {
			t.Errorf("chunkQuoted(%d bytes of %q) = %d chunks, want %d", len(tc.content), tc.content[:1], len(got), tc.chunks)
		}
	}
}

// TestParseListRowsFallsBackToTheBaseName covers a row find printed under a spelling of the directory other than the one asked for, which still names an entry of it.
func TestParseListRowsFallsBackToTheBaseName(t *testing.T) {
	t.Parallel()

	entries := parseListRows("f\t3\t/work/a.txt\nd\t0\t/work/sub\n", "/work/")

	if len(entries) != 2 || entries[0].Name != "a.txt" || entries[1].Name != "sub" || !entries[1].IsDir || entries[0].Size != 3 {
		t.Errorf("parseListRows = %+v, want a.txt (3 bytes) and the directory sub", entries)
	}
}
