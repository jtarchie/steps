package venue

// The deferred fetch and the pull that completes it (steps#138, rung 2).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deferredProducer runs a placed command whose output stays on the worker,
// and reports what the worker kept and where.
func deferredProducer(t *testing.T) (held map[string]string, holder string) {
	t.Helper()

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	spec := localWorker(t, cwd, "out")
	spec.DeferFetch = true
	placed := newLocalRunner(t, spec)

	err := placed.Run(context.Background(), "head -c 1048576 /dev/urandom > out/blob.bin && echo made > out/made.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	_, err = os.Stat(filepath.Join(cwd, "out", "made.txt"))
	if err == nil {
		t.Fatal("the output came home although the fetch was deferred")
	}

	placement, ok := PlacementOf(placed)
	if !ok || placement.BytesReceived != 0 {
		t.Fatalf("BytesReceived = %d, want 0 for a deferred fetch", placement.BytesReceived)
	}

	held, holder, ok = HeldOf(placed)
	if !ok || held["out"] == "" {
		t.Fatalf("HeldOf = %v, want the output under its digest", held)
	}

	return held, holder
}

// TestPullBringsAHeldTreeHome crosses the seam: what one session's worker
// kept, a later Pull to that worker lands here.
func TestPullBringsAHeldTreeHome(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	held, holder := deferredProducer(t)

	dst := filepath.Join(t.TempDir(), "out")
	mustMkdir(t, dst)

	got, err := Pull(context.Background(), localWorker(t, ""), "out", held["out"], dst)
	if err != nil {
		t.Fatalf("Pull from %s: %v", holder, err)
	}

	if got < 1<<20 {
		t.Errorf("Pull reported %d bytes, want at least the megabyte the worker held", got)
	}

	if content := mustRead(t, filepath.Join(dst, "made.txt")); content != "made\n" {
		t.Errorf("made.txt = %q after the pull", content)
	}
}

// TestPullFailsByNameWhenTheWorkerNoLongerHoldsIt: a claim is a claim.
func TestPullFailsByNameWhenTheWorkerNoLongerHoldsIt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)

	held, _ := deferredProducer(t)

	err := os.RemoveAll(filepath.Join(root, "steps-shim", "artifacts"))
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out")
	mustMkdir(t, dst)

	_, err = Pull(context.Background(), localWorker(t, ""), "out", held["out"], dst)
	if err == nil {
		t.Fatal("Pull succeeded from a worker whose cache was emptied")
	}

	if !strings.Contains(err.Error(), `"out"`) {
		t.Errorf("error %q does not name the artifact", err)
	}

	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("dst holds %d entries after a failed pull, want none", len(entries))
	}
}

// TestUnkeptIsWhatStillHasToComeHome pins the fallback: outputs the worker
// could not keep are fetched the ordinary way, and a whole tree is all or
// nothing.
func TestUnkeptIsWhatStillHasToComeHome(t *testing.T) {
	t.Parallel()

	paths, artifact := unkept([]string{"a", "b"}, "", map[string]string{"a": "d"})
	if len(paths) != 1 || paths[0] != "b" || artifact != "" {
		t.Errorf("unkept = %v, %q; want [b], \"\"", paths, artifact)
	}

	paths, artifact = unkept(nil, "src", map[string]string{"src": "d"})
	if len(paths) != 0 || artifact != "" {
		t.Errorf("unkept of a kept tree = %v, %q; want nothing", paths, artifact)
	}

	paths, artifact = unkept(nil, "src", map[string]string{})
	if len(paths) != 0 || artifact != "src" {
		t.Errorf("unkept of an unkept tree = %v, %q; want the tree", paths, artifact)
	}
}
