package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// journalMode reports the sqlite journal mode of the store's database.
func journalMode(t *testing.T, store *Store) string {
	t.Helper()

	var mode string

	err := store.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}

	return mode
}

func TestStoreUsesWAL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	mode := journalMode(t, store)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func mustOpenStore(t *testing.T, path string) *Store {
	t.Helper()

	store, err := OpenStore(path, "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	return store
}

func assertHasSucceeded(t *testing.T, store *Store, jobName, rootHash string, want bool) {
	t.Helper()

	got, err := store.HasSucceeded(context.Background(), jobName, rootHash)
	if err != nil {
		t.Fatalf("HasSucceeded(%q, %q): %v", jobName, rootHash, err)
	}

	if got != want {
		t.Errorf("HasSucceeded(%q, %q) = %v, want %v", jobName, rootHash, got, want)
	}
}

// mustRecordNode records a minimal node under a hash, so a row that references
// it has something to point at.
//
// agent_usage.node_hash and node_transcripts.hash are foreign keys into nodes,
// which is what lets retention delete a node and take its dependents with it.
// job_runs.root_hash deliberately is NOT one (see the schema, and the negative
// assertion in footprint_test.go) — but a job_runs row still names a chain leaf,
// and seeding the node it names keeps these fixtures shaped like the runs that
// produce them.
//
// The node is recorded as SUCCEEDED, which is a cache-hit state: a test about
// HasNodeSucceeded or across-cell memoization should record its own nodes rather
// than inherit this one.
func mustRecordNode(t *testing.T, store *Store, jobName, hash string) {
	t.Helper()

	err := store.RecordNode(context.Background(), NodeRecord{
		Hash: hash, Kind: "task", Resource: "step", Content: map[string]any{"hash": hash},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode(%q, %q): %v", jobName, hash, err)
	}
}

func mustRecordJobRun(t *testing.T, store *Store, jobName, rootHash, status string, runErr error) {
	t.Helper()

	mustRecordNode(t, store, jobName, rootHash)

	err := store.RecordJobRun(context.Background(), jobName, rootHash, status, runErr)
	if err != nil {
		t.Fatalf("RecordJobRun(%q, %q, %q): %v", jobName, rootHash, status, err)
	}
}

func TestStoreHasSucceededAndRecordJobRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.db")

	store := mustOpenStore(t, path)

	_, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("expected parent directory to be created: %v", err)
	}

	assertHasSucceeded(t, store, "job", "hash1", false)

	mustRecordJobRun(t, store, "job", "hash1", "succeeded", nil)
	assertHasSucceeded(t, store, "job", "hash1", true)
	assertHasSucceeded(t, store, "job", "hash2", false)
	assertHasSucceeded(t, store, "other-job", "hash1", false)

	mustRecordJobRun(t, store, "job", "hash3", "failed", errors.New("boom"))
	assertHasSucceeded(t, store, "job", "hash3", false)

	err = store.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := mustOpenStore(t, path)
	defer func() { _ = reopened.Close() }()

	assertHasSucceeded(t, reopened, "job", "hash1", true)
}

func TestStoreHasSucceededBatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	mustRecordJobRun(t, store, "job", "hash1", "succeeded", nil)
	mustRecordJobRun(t, store, "job", "hash2", "failed", errors.New("boom"))
	mustRecordJobRun(t, store, "other-job", "hash1", "succeeded", nil)

	got, err := store.HasSucceededBatch(context.Background(), "job", []string{"hash1", "hash2", "hash3"})
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	want := map[string]bool{"hash1": true}
	if len(got) != len(want) || got["hash1"] != want["hash1"] {
		t.Errorf("HasSucceededBatch = %v, want a map with only hash1=true (hash2 failed, hash3 unknown, other-job's hash1 is a different job)", got)
	}

	got, err = store.HasSucceededBatch(context.Background(), "job", nil)
	if err != nil {
		t.Fatalf("HasSucceededBatch(nil): %v", err)
	}

	if len(got) != 0 {
		t.Errorf("HasSucceededBatch(nil) = %v, want an empty map", got)
	}
}

// TestStoreHasSucceededBatchManyHashes exercises the chunked IN (...) query
// path with more root hashes than fit in a single chunk, confirming it
// neither errors nor drops any of the seeded succeeded rows.
func TestStoreHasSucceededBatchManyHashes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	const n = 1500

	hashes := make([]string, n)

	for i := range n {
		hash := fmt.Sprintf("hash-%d", i)
		hashes[i] = hash

		mustRecordJobRun(t, store, "job", hash, "succeeded", nil)
	}

	got, err := store.HasSucceededBatch(context.Background(), "job", hashes)
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	if len(got) != n {
		t.Fatalf("HasSucceededBatch returned %d entries, want %d", len(got), n)
	}

	for _, hash := range hashes {
		if !got[hash] {
			t.Errorf("HasSucceededBatch[%q] = false, want true", hash)
		}
	}
}

func TestStoreRecordNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	ctx := context.Background()

	node := NodeRecord{
		Hash:       "abc",
		ParentHash: "",
		Kind:       "get",
		StepIndex:  0,
		Resource:   "thing",
		Content:    map[string]any{"source": map[string]any{"key": "v1"}},
	}

	err := store.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	// Recording the same hash again (an upsert) should not error.
	err = store.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode (upsert): %v", err)
	}
}

func mustEnqueueJob(t *testing.T, store *Store, jobName, reason string) {
	t.Helper()

	err := store.EnqueueJob(context.Background(), jobName, reason)
	if err != nil {
		t.Fatalf("EnqueueJob(%q, %q): %v", jobName, reason, err)
	}
}

// mustClaimJob claims the next pending job and fails the test if the queue
// was empty or the claimed job doesn't match want (when want != "").
func mustClaimJob(t *testing.T, store *Store, want string) (id int64, jobName string) {
	t.Helper()

	id, jobName, found, err := store.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if !found {
		t.Fatal("ClaimNextJob: expected a pending job, queue was empty")
	}

	if want != "" && jobName != want {
		t.Fatalf("ClaimNextJob = %q, want %q", jobName, want)
	}

	return id, jobName
}

func assertQueueEmpty(t *testing.T, store *Store) {
	t.Helper()

	_, _, found, err := store.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if found {
		t.Fatal("expected the queue to be empty")
	}
}

func assertLastCheckedVersion(t *testing.T, store *Store, resourceName string, wantFound bool, wantVersion string) {
	t.Helper()

	version, found, err := store.LastCheckedVersion(context.Background(), resourceName)
	if err != nil {
		t.Fatalf("LastCheckedVersion(%q): %v", resourceName, err)
	}

	if found != wantFound || (found && version != wantVersion) {
		t.Fatalf("LastCheckedVersion(%q) = (%q, %v), want (%q, %v)", resourceName, version, found, wantVersion, wantFound)
	}
}

func TestStoreCheckedVersionRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	ctx := context.Background()

	assertLastCheckedVersion(t, store, "thing", false, "")

	err := store.RecordCheckedVersion(ctx, "thing", `{"ref":"v1"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion: %v", err)
	}

	assertLastCheckedVersion(t, store, "thing", true, `{"ref":"v1"}`)

	// Upsert: recording a new version for the same resource replaces it.
	err = store.RecordCheckedVersion(ctx, "thing", `{"ref":"v2"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion (upsert): %v", err)
	}

	assertLastCheckedVersion(t, store, "thing", true, `{"ref":"v2"}`)
}

func TestStoreEnqueueJobDedupsOnlyWhilePending(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	// Two enqueues while nothing has claimed the row yet: one pending row.
	mustEnqueueJob(t, store, "build", "resource-a")
	mustEnqueueJob(t, store, "build", "resource-b")

	mustClaimJob(t, store, "build")
	assertQueueEmpty(t, store)

	// Enqueuing again while the job is running (not pending) creates a fresh
	// pending row — the partial unique index only covers status='pending'.
	mustEnqueueJob(t, store, "build", "resource-c")
}

// TestStoreClaimSerializesSameJob asserts a pending row for a job that is
// already running is not claimable until the running build finishes — builds
// of one job never run concurrently, even with multiple workers.
func TestStoreClaimSerializesSameJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	mustEnqueueJob(t, store, "build", "resource-a")

	id, _ := mustClaimJob(t, store, "build")

	// A change enqueued mid-run: a pending row exists, but it must not be
	// claimable while "build" is still running.
	mustEnqueueJob(t, store, "build", "resource-b")
	assertQueueEmpty(t, store)

	// Once the running build completes, the queued change becomes claimable.
	err := store.CompleteJob(context.Background(), id, "done", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	mustClaimJob(t, store, "build")
}

// TestStoreResetStaleRunningWithPendingSuccessor covers the case a running
// row and a pending row for the same job coexist at crash time: flipping the
// running row to pending would violate idx_trigger_queue_pending_job, so it
// is dropped in favor of the pending successor instead of erroring.
func TestStoreResetStaleRunningWithPendingSuccessor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	mustEnqueueJob(t, store, "build", "resource-a")
	mustClaimJob(t, store, "build")                 // now running
	mustEnqueueJob(t, store, "build", "resource-b") // pending successor

	err := store.ResetStaleRunning(context.Background())
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	// Exactly one claimable build remains (the pending successor); no
	// duplicate, no unique-constraint error.
	mustClaimJob(t, store, "build")
	assertQueueEmpty(t, store)
}

func TestStoreClaimNextJobOrdering(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	jobNames := []string{"job-a", "job-b", "job-c", "job-d", "job-e"}

	for _, name := range jobNames {
		mustEnqueueJob(t, store, name, "resource")
	}

	// Claims must come back oldest-enqueued-first.
	for _, want := range jobNames {
		mustClaimJob(t, store, want)
	}
}

func TestStoreClaimNextJobAtomicity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	const jobCount = 5

	for i := range jobCount {
		mustEnqueueJob(t, store, fmt.Sprintf("job-%d", i), "resource")
	}

	// Concurrent claimers must never both come back with the same job.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = map[string]int{}
	)

	for range jobCount {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, jobName, found, err := store.ClaimNextJob(context.Background())
			if err != nil {
				t.Errorf("ClaimNextJob: %v", err)

				return
			}

			if !found {
				t.Error("expected a job to be claimable")

				return
			}

			mu.Lock()
			claimed[jobName]++
			mu.Unlock()
		}()
	}

	wg.Wait()
	assertClaimedExactlyOnce(t, claimed, jobCount)
}

func assertClaimedExactlyOnce(t *testing.T, claimed map[string]int, wantDistinct int) {
	t.Helper()

	for name, count := range claimed {
		if count != 1 {
			t.Errorf("job %q claimed %d times, want exactly 1", name, count)
		}
	}

	if len(claimed) != wantDistinct {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), wantDistinct)
	}
}

func TestStoreCompleteJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	ctx := context.Background()

	mustEnqueueJob(t, store, "build", "resource")

	id, _ := mustClaimJob(t, store, "build")

	err := store.CompleteJob(ctx, id, "failed", errors.New("boom"))
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	var (
		status  string
		errText *string
	)

	scanErr := store.db.QueryRowContext(ctx, "SELECT status, error FROM trigger_queue WHERE id = ?", id).Scan(&status, &errText)
	if scanErr != nil {
		t.Fatalf("scan trigger_queue row: %v", scanErr)
	}

	if status != "failed" {
		t.Errorf("status = %q, want %q", status, "failed")
	}

	if errText == nil || *errText != "boom" {
		t.Errorf("error = %v, want %q", errText, "boom")
	}
}

func TestStoreResetStaleRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	mustEnqueueJob(t, store, "build", "resource")
	mustClaimJob(t, store, "build")

	// Simulate a crash/interrupted run: the row is stuck "running". A fresh
	// watch startup must recover it, not leave it stranded forever.
	err := store.ResetStaleRunning(context.Background())
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	mustClaimJob(t, store, "build")
}

// TestNodeTranscriptRoundTrip covers the transcript store: absent before any
// save, returned verbatim after, and replaced (not duplicated) on a re-save
// under the same hash — the same replace-on-re-record shape nodes has.
func TestNodeTranscriptRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	ctx := context.Background()

	_, ok, err := store.NodeTranscript(ctx, "abc123")
	if err != nil {
		t.Fatalf("NodeTranscript (empty): %v", err)
	}

	if ok {
		t.Fatal("expected no transcript before any save")
	}

	mustRecordNode(t, store, "build", "abc123")

	err = store.SaveNodeTranscript(ctx, "abc123", `[{"type":"text","text":"hi"}]`)
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	got, ok, err := store.NodeTranscript(ctx, "abc123")
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if got != `[{"type":"text","text":"hi"}]` {
		t.Errorf("transcript = %q", got)
	}
}

// TestNodeTranscriptReplace pins that a re-save under the same hash replaces
// the row rather than duplicating or erroring — the same replace-on-re-record
// shape nodes has.
func TestNodeTranscriptReplace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	ctx := context.Background()

	mustRecordNode(t, store, "build", "abc123")

	for _, transcript := range []string{`[{"type":"text","text":"hi"}]`, `[{"type":"text","text":"replaced"}]`} {
		err := store.SaveNodeTranscript(ctx, "abc123", transcript)
		if err != nil {
			t.Fatalf("SaveNodeTranscript: %v", err)
		}
	}

	got, ok, err := store.NodeTranscript(ctx, "abc123")
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if got != `[{"type":"text","text":"replaced"}]` {
		t.Errorf("replaced transcript = %q", got)
	}
}

// TestConformanceSerialGroupsBlockAcrossJobs verifies serial_groups: matches
// Concourse — two jobs sharing a group never run at the same time, while jobs
// sharing no group are unaffected.
//
// Concourse doc: concourse-ci.org/docs/jobs/ — serial_groups is "a list of
// tags" ensuring jobs with matching tags do not run simultaneously. Written
// spec page, not a source reading.
//
// steps claim under test: internal/config's SerialGroupsByJob, and
// ClaimNextJob's serial-group predicate.
//
// The third job is the control. Without it this would also pass against an
// implementation that simply refused to run any two jobs concurrently, which
// is a different (and much worse) product.
func TestConformanceSerialGroupsBlockAcrossJobs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	err := store.SyncSerialGroups(context.Background(), map[string][]string{
		"deploy-staging": {"deploy"},
		"deploy-prod":    {"deploy"},
		// "lint" belongs to no group at all.
	})
	if err != nil {
		t.Fatalf("SyncSerialGroups: %v", err)
	}

	mustEnqueueJob(t, store, "deploy-staging", "resource-a")
	mustEnqueueJob(t, store, "deploy-prod", "resource-a")
	mustEnqueueJob(t, store, "lint", "resource-a")

	id, first := mustClaimJob(t, store, "deploy-staging")

	// lint shares no group, so it is claimable while a deploy runs.
	_, third := mustClaimJob(t, store, "lint")
	if third != "lint" {
		t.Errorf("claimed %q, want lint — a job in no serial group must not be blocked by one that is", third)
	}

	// deploy-prod shares "deploy" with the running deploy-staging, so it is not.
	assertQueueEmpty(t, store)

	err = store.CompleteJob(context.Background(), id, "done", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	_, second := mustClaimJob(t, store, "deploy-prod")
	if second != "deploy-prod" {
		t.Errorf("claimed %q, want deploy-prod once %q finished", second, first)
	}
}

// TestConformanceMaxInFlightAdmitsUpToTheLimit verifies job-level
// max_in_flight matches Concourse: it caps how many builds of one job run at
// once, rather than the flat one-at-a-time this runner used to enforce.
//
// Concourse doc: concourse-ci.org/docs/jobs/ — "max_in_flight: Specifies a
// maximum number of concurrent builds", with serial:/serial_groups: taking
// precedence and forcing 1.
//
// steps claim under test: config.Job.EffectiveMaxInFlight, Store.SyncMaxInFlight,
// and ClaimNextJob's admission predicate.
//
// This test replaces the guarantee TestConformanceEveryJobIsSerialRegardlessOfConfig
// used to pin, which is why that one is gone: every job being serial was the
// divergence, not the design.
func TestConformanceMaxInFlightAdmitsUpToTheLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	err := store.SyncMaxInFlight(context.Background(), map[string]int{"build": 2})
	if err != nil {
		t.Fatalf("SyncMaxInFlight: %v", err)
	}

	// Three changes queue up. Only one row may be PENDING per job at a time,
	// so each is claimed before the next is enqueued — which is exactly how a
	// real watcher reaches two concurrent builds of one job.
	mustEnqueueJob(t, store, "build", "resource-a")
	first, _ := mustClaimJob(t, store, "build")

	mustEnqueueJob(t, store, "build", "resource-b")
	mustClaimJob(t, store, "build")

	// Two running, limit is two: the third must wait.
	mustEnqueueJob(t, store, "build", "resource-c")
	assertQueueEmpty(t, store)

	// One finishes, so a slot opens.
	err = store.CompleteJob(context.Background(), first, "done", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	mustClaimJob(t, store, "build")
}

// TestConformanceSerialForcesOneInFlight pins the precedence rule: serial:
// wins over any max_in_flight, which is why config rejects setting both.
func TestConformanceSerialForcesOneInFlight(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	// What EffectiveMaxInFlight produces for a serial job.
	err := store.SyncMaxInFlight(context.Background(), map[string]int{"deploy": 1})
	if err != nil {
		t.Fatalf("SyncMaxInFlight: %v", err)
	}

	mustEnqueueJob(t, store, "deploy", "resource-a")
	mustClaimJob(t, store, "deploy")

	mustEnqueueJob(t, store, "deploy", "resource-b")
	assertQueueEmpty(t, store)
}

// TestMaxInFlightDefaultsToOneForAnUnknownJob covers the row that is not
// there: a job removed from the pipeline between enqueue and claim.
//
// COALESCE defaults it to 1 rather than to unlimited, because serializing
// something nobody can describe is the conservative reading — and because the
// alternative would turn a config typo into unbounded concurrency.
func TestMaxInFlightDefaultsToOneForAnUnknownJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	// Nothing synced at all.
	mustEnqueueJob(t, store, "ghost", "resource-a")
	mustClaimJob(t, store, "ghost")

	mustEnqueueJob(t, store, "ghost", "resource-b")
	assertQueueEmpty(t, store)
}

// TestConsumedMarkRoundTrip covers the get: version: every cursor: how far a
// job has fanned out, per (job, resource).
func TestConsumedMarkRoundTrip(t *testing.T) {
	t.Parallel()

	st := openTestStore(t)
	ctx := context.Background()

	mark, err := st.ConsumedMark(ctx, "answer", "mentions")
	if err != nil {
		t.Fatalf("ConsumedMark: %v", err)
	}

	if mark != 0 {
		t.Fatalf("a fresh store reports mark %d, want 0 — nothing taken", mark)
	}

	err = st.RecordConsumedMark(ctx, "answer", "mentions", 5)
	if err != nil {
		t.Fatalf("RecordConsumedMark: %v", err)
	}

	// Re-recording is a no-op, not an error: a resumed or replayed run must
	// not fail on work it already did.
	err = st.RecordConsumedMark(ctx, "answer", "mentions", 5)
	if err != nil {
		t.Fatalf("RecordConsumedMark (again): %v", err)
	}

	// And it only moves forward. A backfilled version reaching a job that has
	// already moved past it must not rewind the mark and hand back everything
	// in between.
	err = st.RecordConsumedMark(ctx, "answer", "mentions", 2)
	if err != nil {
		t.Fatalf("RecordConsumedMark (older): %v", err)
	}

	mark, err = st.ConsumedMark(ctx, "answer", "mentions")
	if err != nil {
		t.Fatalf("ConsumedMark: %v", err)
	}

	if mark != 5 {
		t.Errorf("mark = %d, want 5 — an older version must not rewind it", mark)
	}

	// Scoped per job and per resource — another job has taken nothing.
	other, err := st.ConsumedMark(ctx, "other-job", "mentions")
	if err != nil {
		t.Fatalf("ConsumedMark: %v", err)
	}

	if other != 0 {
		t.Errorf("another job reports mark %d; the cursor is per job", other)
	}
}
