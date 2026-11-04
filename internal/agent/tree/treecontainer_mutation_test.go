package tree

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// treecontainerModeRunner records, per script, whether it was run as a read.
type treecontainerModeRunner struct {
	shell.Runner
	readOnly []bool
}

func (r *treecontainerModeRunner) Exec(ctx context.Context, _ shell.Command) (shell.Result, error) {
	r.readOnly = append(r.readOnly, shell.IsReadOnly(ctx))

	return shell.Result{}, nil
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

// scriptedRunner answers every script with one canned result and records what it was asked to run.
type scriptedRunner struct {
	shell.Runner

	result  shell.Result
	err     error
	scripts []string
}

func (r *scriptedRunner) Exec(_ context.Context, cmd shell.Command) (shell.Result, error) {
	r.scripts = append(r.scripts, cmd.Script)

	return r.result, r.err
}

// A line limit becomes a sed range inside the container, so a range deep in a large file crosses the wire as the range; no limit reads the head of the file whole.
func TestContainerOpenRangeAsksSedOnlyWhenThereIsALimit(t *testing.T) {
	t.Parallel()

	for through, wantSed := range map[int]bool{3: true, 0: false} {
		runner := &scriptedRunner{result: shell.Result{Stdout: "line1\nline2\n"}}
		tree := Container{Runner: runner, dir: "/work", Lost: &Lost{}}

		body, err := tree.OpenRange(t.Context(), "/work/f.txt", through)
		if err != nil {
			t.Fatalf("OpenRange(through %d): %v", through, err)
		}

		got, err := io.ReadAll(body)
		if err != nil || string(got) != "line1\nline2\n" {
			t.Errorf("OpenRange(through %d) read %q, %v; want what the script printed", through, got, err)
		}

		script := runner.scripts[0]
		if strings.Contains(script, "sed -n") != wantSed {
			t.Errorf("OpenRange(through %d) ran %q; a sed range is wanted exactly when there is a limit (%v)", through, script, wantSed)
		}

		if wantSed && !strings.Contains(script, "sed -n '1,3p'") {
			t.Errorf("OpenRange(through %d) ran %q, want the range 1,3", through, script)
		}

		if !strings.Contains(script, "head -c") {
			t.Errorf("OpenRange(through %d) ran %q, want its output bounded by head -c", through, script)
		}
	}
}

// list_dir reads the same entries wherever the agent ran: sorted by name, with each row's kind and size.
func TestContainerListDirReturnsTheRowsSortedByName(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{result: shell.Result{Stdout: "f\t5\t/work/d/b.txt\nd\t0\t/work/d/a\nf\t1\t/work/d/c.txt\n"}}
	tree := Container{Runner: runner, dir: "/work", Lost: &Lost{}}

	entries, err := tree.ListDir(t.Context(), "/work/d")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}

	want := []Entry{{Name: "a", IsDir: true}, {Name: "b.txt", Size: 5}, {Name: "c.txt", Size: 1}}
	if len(entries) != len(want) {
		t.Fatalf("ListDir = %+v, want %+v", entries, want)
	}

	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

// A path the script cannot find is the script's own answer (a nonzero exit): not-exist, never a lost container.
func TestContainerOpenRangeAndListDirReportAMissingPath(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{result: shell.Result{Code: 1}}
	lost := &Lost{}
	tree := Container{Runner: runner, dir: "/work", Lost: lost}

	_, err := tree.OpenRange(t.Context(), "/work/nope", 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenRange of a missing file = %v, want os.ErrNotExist", err)
	}

	_, err = tree.ListDir(t.Context(), "/work/nope")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ListDir of a missing directory = %v, want os.ErrNotExist", err)
	}

	if lost.Taken() != nil {
		t.Errorf("a missing path marked the tree lost: %v", lost.Taken())
	}
}

// A runner that cannot run the script at all is a lost container: the error says so, and the step is told.
func TestContainerOpenRangeAndListDirReportARunnerFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("container went away")
	runner := &scriptedRunner{err: boom}
	lost := &Lost{}
	tree := Container{Runner: runner, dir: "/work", Lost: lost}

	_, err := tree.OpenRange(t.Context(), "/work/f.txt", 0)
	if !errors.Is(err, ErrNoAccess) || !errors.Is(err, boom) {
		t.Errorf("OpenRange on a failing runner = %v, want ErrNoAccess wrapping the runner's error", err)
	}

	_, err = tree.ListDir(t.Context(), "/work")
	if !errors.Is(err, ErrNoAccess) || !errors.Is(err, boom) {
		t.Errorf("ListDir on a failing runner = %v, want ErrNoAccess wrapping the runner's error", err)
	}

	if lost.Taken() == nil {
		t.Error("a runner failure did not mark the tree lost")
	}
}
