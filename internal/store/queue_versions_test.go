package store

// The versions a poll attaches to a queue row, and the two places they could
// otherwise be silently lost.

import (
	"context"
	"path/filepath"
	"testing"
)

func versionsOf(t *testing.T, store *Store, id int64) QueuedVersions {
	t.Helper()

	versions, err := store.ClaimedVersions(context.Background(), id)
	if err != nil {
		t.Fatalf("ClaimedVersions: %v", err)
	}

	return versions
}

// TestEnqueueMergesVersionsOnCollision is the rule that makes it safe to put
// work on a queue row at all. At most one pending row exists per job, so a
// second poll before a worker claims the first used to be dropped outright —
// free when the job re-derived everything for itself, data loss now that the
// row IS the work, since steps keeps no version history to recover from.
func TestEnqueueMergesVersionsOnCollision(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	err := store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{
		"items": {{"n": "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// A second poll, before anything claimed the row.
	err = store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{
		"items": {{"n": "2"}},
		"other": {{"n": "9"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	id, _, found, err := store.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("ClaimNextJob: %v found=%v", err, found)
	}

	got := versionsOf(t, store, id)

	if len(got["items"]) != 2 || got["items"][0]["n"] != "1" || got["items"][1]["n"] != "2" {
		t.Errorf("items = %+v, want both polls' versions, oldest first", got["items"])
	}

	if len(got["other"]) != 1 {
		t.Errorf("other = %+v, want the resource the second poll added", got["other"])
	}
}

// TestEnqueueMergeDedupesVersions: the same version reported twice is one
// piece of work, matched on the canonical JSON the version tables already key
// on.
func TestEnqueueMergeDedupesVersions(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	for range 3 {
		err := store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{
			"items": {{"n": "1"}, {"n": "2"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	id, _, _, err := store.ClaimNextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if got := versionsOf(t, store, id); len(got["items"]) != 2 {
		t.Errorf("items = %+v, want 2 after three identical enqueues", got["items"])
	}
}

// TestEnqueueWithoutVersionsLeavesThemAlone: a hand-queued row (the web UI, a
// manual re-run) must not blank out versions a poll already attached.
func TestEnqueueWithoutVersionsLeavesThemAlone(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	err := store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{"items": {{"n": "1"}}})
	if err != nil {
		t.Fatal(err)
	}

	err = store.EnqueueJob(ctx, "build", "manual")
	if err != nil {
		t.Fatal(err)
	}

	id, _, _, err := store.ClaimNextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if got := versionsOf(t, store, id); len(got["items"]) != 1 {
		t.Errorf("items = %+v, want the poll's version to survive a plain enqueue", got["items"])
	}
}

// TestResetStaleRunningKeepsVersions covers a restart. A crashed run's row is
// superseded by the pending one queued behind it and deleted — which used to
// be free, because the job would re-derive its versions. Now the row carries
// them, so they have to be folded into the survivor or that work is gone.
func TestResetStaleRunningKeepsVersions(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	// A poll queues item 1; a worker claims it and the process dies.
	err := store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{"items": {{"n": "1"}}})
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = store.ClaimNextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A later poll queues item 2 behind it.
	err = store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{"items": {{"n": "2"}}})
	if err != nil {
		t.Fatal(err)
	}

	// Restart.
	err = store.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatal(err)
	}

	id, _, found, err := store.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("ClaimNextJob after restart: %v found=%v", err, found)
	}

	got := versionsOf(t, store, id)
	if len(got["items"]) != 2 {
		t.Fatalf("items = %+v, want both — the interrupted run's version must not be dropped", got["items"])
	}

	assertQueueEmpty(t, store)
}
