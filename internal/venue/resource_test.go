package venue

// The two shapes a resource stage adds to the venue: a command whose output
// is the whole tree (an in:), and a command with no tree at all (a check:).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestVenueFetchAllBringsTheWholeTreeHome: an in: fills an empty directory
// and declares nothing, so everything it wrote has to come back.
func TestVenueFetchAllBringsTheWholeTreeHome(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()

	spec := localWorker(t, cwd)
	spec.FetchAll = true

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "mkdir -p nested && echo one > top.txt && echo two > nested/deep.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "top.txt")); got != "one\n" {
		t.Errorf("top.txt = %q, want the file the worker wrote at the root", got)
	}

	if got := mustRead(t, filepath.Join(cwd, "nested", "deep.txt")); got != "two\n" {
		t.Errorf("nested/deep.txt = %q, want the directory the worker created", got)
	}

	// No staging leftovers: the swap moved everything and removed its own
	// scratch.
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.Name() != "top.txt" && entry.Name() != "nested" {
			t.Errorf("unexpected entry %q left in the tree", entry.Name())
		}
	}
}

// TestVenueNoTreeRunsACommand: a check: has no directory, here or there. The
// session must open without one, run, and capture — nothing to send, nothing
// to bring back.
func TestVenueNoTreeRunsACommand(t *testing.T) {
	t.Parallel()

	spec := localWorker(t, "")
	spec.WorkerTag = "vpc"

	runner := newLocalRunner(t, spec)

	out, err := runner.RunCapture(context.Background(), `printf '[{"where":"%s"}]' "$STEPS_WORKER"`)
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}

	if string(out) != `[{"where":"vpc"}]` {
		t.Errorf("stdout = %q, want the command's answer, tagged", out)
	}
}

// TestVenueFetchAllRemovesWhatTheWorkerDeleted: the tree IS the output, so a
// local entry the command removed on the worker goes too — or a retried in:
// leaves a union of attempts for the resource cache to keep.
func TestVenueFetchAllRemovesWhatTheWorkerDeleted(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "stale.tgz"), "leftover\n")

	spec := localWorker(t, cwd)
	spec.FetchAll = true

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "rm stale.tgz && echo fresh > src.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	_, err = os.Stat(filepath.Join(cwd, "stale.tgz"))
	if err == nil {
		t.Error("stale.tgz survived a fetch of a tree the worker had deleted it from")
	}

	if got := mustRead(t, filepath.Join(cwd, "src.txt")); got != "fresh\n" {
		t.Errorf("src.txt = %q, want the worker's file", got)
	}
}
