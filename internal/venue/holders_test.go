package venue

// The produce-then-consume seam (steps#138): a tree one session brought home
// is offered by the next session, and the worker answers "already here".
//
// The two halves — the shim filing what it packs, this end digesting what it
// offers — agree only if they name the same bytes. Nothing but a test that
// crosses from one session's fetch into another's upload can prove that.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

func sentBytes(t *testing.T, placed shell.Runner) int64 {
	t.Helper()

	remote, ok := placed.(runner)
	if !ok {
		t.Fatalf("runner is %T, not a placed one", placed)
	}

	return remote.session.sentArtifactBytes.Load()
}

// produceThenConsume runs a producer whose declared output holds the payload,
// then a consumer whose tree has that output as an input, and reports what
// the consumer's session sent.
func produceThenConsume(t *testing.T, store string) int64 {
	t.Helper()

	producerCwd := t.TempDir()
	mustMkdir(t, filepath.Join(producerCwd, "out"))

	spec := localWorker(t, producerCwd, "out")
	spec.ArtifactStore = store
	producer := newLocalRunner(t, spec)

	// Made ON the worker, incompressible: the producer's own tree must carry
	// nothing, or the store test counts the producer's upload as the
	// consumer's re-fetch.
	err := producer.Run(context.Background(), "head -c 1048576 /dev/urandom > out/blob.bin")
	if err != nil {
		t.Fatalf("producer Run: %v", err)
	}

	consumerCwd := t.TempDir()

	// Rename, not copy: what the next step materializes is the fetched
	// directory as it is, mode and all.
	err = os.Rename(filepath.Join(producerCwd, "out"), filepath.Join(consumerCwd, "out"))
	if err != nil {
		t.Fatalf("handing the output to the consumer: %v", err)
	}

	spec = localWorker(t, consumerCwd, "result")
	spec.ArtifactStore = store
	mustMkdir(t, filepath.Join(consumerCwd, "result"))

	consumer := newLocalRunner(t, spec)

	err = consumer.Run(context.Background(), "wc -c < out/blob.bin > result/n")
	if err != nil {
		t.Fatalf("consumer Run: %v", err)
	}

	if got := mustRead(t, filepath.Join(consumerCwd, "result", "n")); got != "1048576\n" && got != " 1048576\n" {
		t.Fatalf("the consumer read %q, want the payload's size", got)
	}

	return sentBytes(t, consumer)
}

// TestTunnelDoesNotResendWhatTheWorkerProduced is the tunnel half of the seam.
func TestTunnelDoesNotResendWhatTheWorkerProduced(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	if sent := produceThenConsume(t, ""); sent > 1<<19 {
		t.Errorf("the consumer sent %d bytes for an output the worker itself produced, want almost none", sent)
	}
}

// TestStoreDoesNotReuploadWhatTheWorkerProduced is the store half: the
// orchestrator still puts the blob (the store is the fleet's cache), but the
// worker never pulls it.
func TestStoreDoesNotReuploadWhatTheWorkerProduced(t *testing.T) {
	fake, storeURL := newCountingS3(t)

	produceThenConsume(t, storeURL)

	if fake.treeBytesOut > 1<<19 {
		t.Errorf("the worker pulled %d bytes of tree it had itself produced, want almost none", fake.treeBytesOut)
	}
}

// TestTunnelDoesNotResendAFetchedResource is the get's shape: the whole work
// directory comes home as one artifact, and the next step offers it under
// that name.
func TestTunnelDoesNotResendAFetchedResource(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	build := t.TempDir()

	resourceDir := filepath.Join(build, "artifacts", "src")
	mustMkdir(t, resourceDir)

	spec := localWorker(t, resourceDir)
	spec.FetchAll = true
	fetch := newLocalRunner(t, spec)

	err := fetch.Run(context.Background(), "head -c 1048576 /dev/urandom > blob.bin && echo v1 > version")
	if err != nil {
		t.Fatalf("fetch Run: %v", err)
	}

	stepDir := filepath.Join(build, "steps", "01-consume")
	mustMkdir(t, stepDir)

	err = os.Rename(resourceDir, filepath.Join(stepDir, "src"))
	if err != nil {
		t.Fatalf("materializing the resource for the consumer: %v", err)
	}

	mustMkdir(t, filepath.Join(stepDir, "result"))
	consumer := newLocalRunner(t, localWorker(t, stepDir, "result"))

	err = consumer.Run(context.Background(), "cat src/version > result/v")
	if err != nil {
		t.Fatalf("consumer Run: %v", err)
	}

	if sent := sentBytes(t, consumer); sent > 1<<19 {
		t.Errorf("the consumer sent %d bytes for a resource the worker itself fetched, want almost none", sent)
	}
}

// TestSessionCountsWhatItBringsHome is the other column of the ledger: what
// the worker produced and this end read back, on both planes.
func TestSessionCountsWhatItBringsHome(t *testing.T) {
	for _, plane := range []struct {
		name  string
		store func(t *testing.T) string
	}{
		{name: "tunnel", store: func(t *testing.T) string { t.Helper(); t.Setenv("TMPDIR", t.TempDir()); return "" }},
		{name: "store", store: func(t *testing.T) string { t.Helper(); _, url := newCountingS3(t); return url }},
	} {
		t.Run(plane.name, func(t *testing.T) {
			cwd := t.TempDir()
			mustMkdir(t, filepath.Join(cwd, "out"))

			spec := localWorker(t, cwd, "out")
			spec.ArtifactStore = plane.store(t)
			placed := newLocalRunner(t, spec)

			err := placed.Run(context.Background(), "head -c 1048576 /dev/urandom > out/blob.bin")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			placement, ok := PlacementOf(placed)
			if !ok {
				t.Fatal("no placement for a runner that ran")
			}

			if placement.BytesReceived < 1<<20 {
				t.Errorf("BytesReceived = %d, want at least the megabyte the worker produced", placement.BytesReceived)
			}
		})
	}
}
