package venue

// What a runner will and will not say about the machine it used.

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestPlacementIsSilentBeforeTheHandshake: a session dials LAZILY, so a
// runner that was built and never asked to run anything has met no machine.
// Answering with a half-filled struct there would record a placement that
// never happened — an empty platform and a zero byte count reading as facts
// about a worker nobody talked to.
func TestPlacementIsSilentBeforeTheHandshake(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	runner := newLocalRunner(t, localWorker(t, cwd))

	if placement, ok := PlacementOf(runner); ok {
		t.Errorf("PlacementOf before any command reported %+v, want nothing — the session has not dialed", placement)
	}

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	placement, ok := PlacementOf(runner)
	if !ok {
		t.Fatal("PlacementOf after a command reported nothing — the worker described itself and it was dropped")
	}

	if placement.GOOS != runtime.GOOS || placement.GOARCH != runtime.GOARCH {
		t.Errorf("platform = %s/%s, want the local: worker's own %s/%s",
			placement.GOOS, placement.GOARCH, runtime.GOOS, runtime.GOARCH)
	}

	if placement.Workdir == "" || placement.FSType == "" {
		t.Errorf("workdir %q on fstype %q — both come from the worker's hello and both should be there",
			placement.Workdir, placement.FSType)
	}
}

// TestPlacementCountsWhatWasPushed: BytesSent is the number the per-artifact
// grain exists to reduce, and nothing outside the session can weigh it — the
// tunnel is a pipe to a process.
func TestPlacementCountsWhatWasPushed(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	// Content nothing has pushed before: a worker KEEPS what it receives and
	// answers a second offer of the same artifact with "already held", so a
	// fixed string would legitimately cost zero bytes and the assertion below
	// would be measuring the cache rather than the counter.
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), cwd+"\n")

	runner := newLocalRunner(t, localWorker(t, cwd))

	err := runner.Run(context.Background(), "test -s data/seed.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	placement, ok := PlacementOf(runner)
	if !ok {
		t.Fatal("PlacementOf reported nothing after a command ran")
	}

	if placement.BytesSent <= 0 {
		t.Errorf("bytes_sent = %d, want the tree that was pushed", placement.BytesSent)
	}
}

// TestPlacementOfAnUnplacedRunnerIsFalse, so a caller need not first ask
// whether it is holding a venue.
func TestPlacementOfAnUnplacedRunnerIsFalse(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(shell.RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	defer func() { _ = runner.Close() }()

	if _, ok := PlacementOf(runner); ok {
		t.Error("a local runner reported a placement — there is no worker to describe")
	}
}

// TestPlacementCountsWhatTheStorePlanePushed is the same promise on the other
// plane — the one aws:// actually uses, since --artifact-store is mandatory
// for a `?binary=` worker.
//
// The counter had exactly one writer, on the tunnel, so a placement made
// through the store reported 0 B structurally. Every reader then rendered a
// lie in the honest direction's own words: the run page documents 0 B as "a
// step whose inputs were already there", which made a cold worker that pulled
// the entire tree indistinguishable from a perfect cache hit.
func TestPlacementCountsWhatTheStorePlanePushed(t *testing.T) {
	_, storeURL := newCountingS3(t)

	cwd := t.TempDir()
	// Content nothing has pushed before, for the reason the tunnel twin
	// gives: the store is content-addressed, and a fixed string would already
	// be there.
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), cwd+"\n")

	spec := localWorker(t, cwd)
	spec.ArtifactStore = storeURL

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "test -s data/seed.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	placement, ok := PlacementOf(runner)
	if !ok {
		t.Fatal("PlacementOf reported nothing after a command ran")
	}

	if placement.BytesSent <= 0 {
		t.Errorf("bytes_sent = %d after a tree demonstrably moved through the store, want what was pushed into it",
			placement.BytesSent)
	}
}
