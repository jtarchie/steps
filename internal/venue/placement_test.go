package venue

// What a runner will and will not say about the machine it used.

import (
	"context"
	"path/filepath"
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

	// The daemon's platform, not this process's: a local: worker on a Mac runs linux containers.
	if placement.GOOS != "linux" || placement.GOARCH == "" {
		t.Errorf("platform = %s/%s, want the daemon's linux/<arch>", placement.GOOS, placement.GOARCH)
	}

	if placement.Workdir == "" {
		t.Error("workdir is empty — the placement should say where the step ran")
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
