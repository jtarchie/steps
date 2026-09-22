package venue

// The deferred fetch and the pull that completes it (steps#138, rung 2).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
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

// TestARemoteInputReachesTheWorkerThroughTheStore crosses the rung 3 seam:
// worker a keeps a tree, a session on worker b names it as a remote input,
// and b reads it — a pushed it to the store, this end put nothing.
func TestARemoteInputReachesTheWorkerThroughTheStore(t *testing.T) {
	fake, storeURL := newCountingS3(t)

	// Two roots, so a and b are two caches; the store is the only thing they
	// share.
	rootA := t.TempDir()
	rootB := t.TempDir()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	producerCwd := t.TempDir()
	mustMkdir(t, filepath.Join(producerCwd, "out"))

	producer := newLocalRunner(t, shell.RunnerSpec{
		Cwd: producerCwd, Worker: "local:" + rootA + "?binary=" + self, Fetch: []string{"out"},
		DeferFetch: true, ArtifactStore: storeURL,
	})

	err = producer.Run(context.Background(), "head -c 1048576 /dev/urandom > out/blob.bin")
	if err != nil {
		t.Fatalf("producer: %v", err)
	}

	held, holder, ok := HeldOf(producer)
	if !ok {
		t.Fatal("the producer's worker kept nothing")
	}

	consumerCwd := t.TempDir()
	mustMkdir(t, filepath.Join(consumerCwd, "result"))

	consumer := newLocalRunner(t, shell.RunnerSpec{
		Cwd: consumerCwd, Worker: "local:" + rootB + "?binary=" + self, Fetch: []string{"result"},
		ArtifactStore: storeURL,
		RemoteInputs:  map[string]shell.RemoteInput{"out": {Digest: held["out"], Holder: holder}},
	})

	err = consumer.Run(context.Background(), "wc -c < out/blob.bin | tr -d ' ' > result/n")
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	if got := strings.TrimSpace(mustRead(t, filepath.Join(consumerCwd, "result", "n"))); got != "1048576" {
		t.Errorf("the consumer read %q of the remote input", got)
	}

	if sent := sentBytes(t, consumer); sent > 1<<19 {
		t.Errorf("this end put %d bytes for an input it never held", sent)
	}

	// Three, and which three is the point: the producer's empty output
	// directory going out, the megabyte a pushed, and the consumer's empty
	// output directory — never the megabyte from this end.
	if fake.treePuts != 3 {
		t.Errorf("the store took %d tree puts, want 3", fake.treePuts)
	}
}
