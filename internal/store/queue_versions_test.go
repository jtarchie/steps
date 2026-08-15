package store

// The versions a poll attaches to a queue row, and the two places they could
// otherwise be silently lost.

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestManualEnqueueClearsPolledVersions: pressing Run asks for the job to be
// run now, against whatever is current. Inheriting the versions a poll
// happened to observe earlier answers a different question — and a confusing
// one, since the button is often pressed precisely because those versions
// looked wrong.
func TestManualEnqueueClearsPolledVersions(t *testing.T) {
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

	if got := versionsOf(t, store, id); len(got) != 0 {
		t.Errorf("items = %+v, want none — a manual run resolves its own versions", got)
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

// TestQueuedVersionsKeepTheirDigits: a version goes straight back into a
// template and out over the wire, so encoding/json's default float64 is not
// good enough — a Slack ts renders as 1.6998876540012e+09 and an id wider
// than float64 stops being the id. resource.ParseVersionJSON exists for this
// reason on the poll side; the queue has to make the same promise, or a
// triggered run sends an API a value it has never seen while a manual run of
// the same pipeline works.
func TestQueuedVersionsKeepTheirDigits(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	err := store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{
		"items": {{"id": json.Number("1234567890123456789"), "ts": json.Number("1699887654.001200")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	id, _, _, err := store.ClaimNextJob(ctx)
	if err != nil {
		t.Fatal(err)
	}

	version := versionsOf(t, store, id)["items"][0]

	if got := fmt.Sprint(version["id"]); got != "1234567890123456789" {
		t.Errorf("id = %s, want the digits as written", got)
	}

	if got := fmt.Sprint(version["ts"]); got != "1699887654.001200" {
		t.Errorf("ts = %s, want the digits as written", got)
	}
}

// TestResetStaleRunningWithSeveralBuildsInFlight covers a job whose
// max_in_flight lets more than one build run at once — which is where the old
// "flip every running row to pending" broke in two ways at once.
//
// It produced two pending rows for one job, violating the partial unique
// index, so ResetStaleRunning failed outright: a watcher that crashed with
// two builds in flight could never start again. And when it did not fail, it
// kept only one row's versions.
func TestResetStaleRunningWithSeveralBuildsInFlight(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	err := store.SyncMaxInFlight(ctx, map[string]int{"build": 2})
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"1", "2"} {
		err = store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{"items": {{"n": n}}})
		if err != nil {
			t.Fatal(err)
		}

		_, _, _, err = store.ClaimNextJob(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}

	// A third version queued behind the two in flight.
	err = store.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{"items": {{"n": "3"}}})
	if err != nil {
		t.Fatal(err)
	}

	err = store.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v — a watcher that crashed mid-flight could not restart", err)
	}

	id, _, found, err := store.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("ClaimNextJob after restart: %v found=%v", err, found)
	}

	if got := versionsOf(t, store, id)["items"]; len(got) != 3 {
		t.Errorf("%d versions survived the restart, want 3: %+v", len(got), got)
	}
}
