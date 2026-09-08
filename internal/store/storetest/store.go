package storetest

// The cache, the queue and the job limits — the facets a build runs on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

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
func mustRecordNode(t *testing.T, st store.Store, jobName, hash string) {
	t.Helper()

	err := st.RecordNode(context.Background(), store.NodeRecord{
		Hash: hash, Kind: "task", Resource: "step", Content: map[string]any{"hash": hash},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode(%q, %q): %v", jobName, hash, err)
	}
}

func mustRecordJobRun(t *testing.T, st store.Store, jobName, rootHash, status string, runErr error) {
	t.Helper()

	mustRecordNode(t, st, jobName, rootHash)

	err := st.RecordJobRun(context.Background(), jobName, rootHash, status, runErr)
	if err != nil {
		t.Fatalf("RecordJobRun(%q, %q, %q): %v", jobName, rootHash, status, err)
	}
}

func (s suite) TestStoreHasSucceededBatch(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustRecordJobRun(t, st, "job", "hash1", "succeeded", nil)
	mustRecordJobRun(t, st, "job", "hash2", "failed", errors.New("boom"))
	mustRecordJobRun(t, st, "other-job", "hash1", "succeeded", nil)

	got, err := st.HasSucceededBatch(context.Background(), "job", []string{"hash1", "hash2", "hash3"})
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	want := map[string]bool{"hash1": true}
	if len(got) != len(want) || got["hash1"] != want["hash1"] {
		t.Errorf("HasSucceededBatch = %v, want a map with only hash1=true (hash2 failed, hash3 unknown, other-job's hash1 is a different job)", got)
	}

	got, err = st.HasSucceededBatch(context.Background(), "job", nil)
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
func (s suite) TestStoreHasSucceededBatchManyHashes(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	const n = 1500

	hashes := make([]string, n)

	for i := range n {
		hash := fmt.Sprintf("hash-%d", i)
		hashes[i] = hash

		mustRecordJobRun(t, st, "job", hash, "succeeded", nil)
	}

	got, err := st.HasSucceededBatch(context.Background(), "job", hashes)
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

func (s suite) TestStoreRecordNode(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	node := store.NodeRecord{
		Hash:       "abc",
		ParentHash: "",
		Kind:       "get",
		StepIndex:  0,
		Resource:   "thing",
		Content:    map[string]any{"source": map[string]any{"key": "v1"}},
	}

	err := st.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	// Recording the same hash again (an upsert) should not error.
	err = st.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode (upsert): %v", err)
	}
}

func mustEnqueueJob(t *testing.T, st store.Store, jobName, reason string) {
	t.Helper()

	err := st.EnqueueJob(context.Background(), jobName, reason)
	if err != nil {
		t.Fatalf("EnqueueJob(%q, %q): %v", jobName, reason, err)
	}
}

// mustClaimJob claims the next pending job and fails the test if the queue
// was empty or the claimed job doesn't match want (when want != "").
func mustClaimJob(t *testing.T, st store.Store, want string) (id int64, jobName string) {
	t.Helper()

	id, jobName, found, err := st.ClaimNextJob(context.Background())
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

func assertQueueEmpty(t *testing.T, st store.Store) {
	t.Helper()

	_, _, found, err := st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if found {
		t.Fatal("expected the queue to be empty")
	}
}

func assertLastCheckedVersion(t *testing.T, st store.Store, resourceName string, wantFound bool, wantVersion string) {
	t.Helper()

	last, found, err := st.LastChecked(context.Background(), resourceName)
	if err != nil {
		t.Fatalf("LastChecked(%q): %v", resourceName, err)
	}

	if found != wantFound || (found && last.Version != wantVersion) {
		t.Fatalf("LastChecked(%q) = (%q, %v), want (%q, %v)", resourceName, last.Version, found, wantVersion, wantFound)
	}
}

func (s suite) TestStoreCheckedVersionRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	assertLastCheckedVersion(t, st, "thing", false, "")

	err := st.RecordCheckedVersion(ctx, "thing", `{"ref":"v1"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion: %v", err)
	}

	assertLastCheckedVersion(t, st, "thing", true, `{"ref":"v1"}`)

	// Upsert: recording a new version for the same resource replaces it.
	err = st.RecordCheckedVersion(ctx, "thing", `{"ref":"v2"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion (upsert): %v", err)
	}

	assertLastCheckedVersion(t, st, "thing", true, `{"ref":"v2"}`)
}

func (s suite) TestStoreEnqueueJobDedupsOnlyWhilePending(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	// Two enqueues while nothing has claimed the row yet: one pending row.
	mustEnqueueJob(t, st, "build", "resource-a")
	mustEnqueueJob(t, st, "build", "resource-b")

	mustClaimJob(t, st, "build")
	assertQueueEmpty(t, st)

	// Enqueuing again while the job is running (not pending) creates a fresh
	// pending row — the partial unique index only covers status='pending'.
	mustEnqueueJob(t, st, "build", "resource-c")
}

// TestStoreClaimSerializesSameJob asserts a pending row for a job that is
// already running is not claimable until the running build finishes — builds
// of one job never run concurrently, even with multiple workers.
func (s suite) TestStoreClaimSerializesSameJob(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustEnqueueJob(t, st, "build", "resource-a")

	id, _ := mustClaimJob(t, st, "build")

	// A change enqueued mid-run: a pending row exists, but it must not be
	// claimable while "build" is still running.
	mustEnqueueJob(t, st, "build", "resource-b")
	assertQueueEmpty(t, st)

	// Once the running build completes, the queued change becomes claimable.
	err := st.CompleteJob(context.Background(), id, "done", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	mustClaimJob(t, st, "build")
}

// TestStoreResetStaleRunningWithPendingSuccessor covers the case a running
// row and a pending row for the same job coexist at crash time: flipping the
// running row to pending would violate idx_trigger_queue_pending_job, so it
// is dropped in favor of the pending successor instead of erroring.
func (s suite) TestStoreResetStaleRunningWithPendingSuccessor(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustEnqueueJob(t, st, "build", "resource-a")
	mustClaimJob(t, st, "build")                 // now running
	mustEnqueueJob(t, st, "build", "resource-b") // pending successor

	err := st.ResetStaleRunning(context.Background())
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	// Exactly one claimable build remains (the pending successor); no
	// duplicate, no unique-constraint error.
	mustClaimJob(t, st, "build")
	assertQueueEmpty(t, st)
}

func (s suite) TestStoreClaimNextJobOrdering(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	jobNames := []string{"job-a", "job-b", "job-c", "job-d", "job-e"}

	for _, name := range jobNames {
		mustEnqueueJob(t, st, name, "resource")
	}

	// Claims must come back oldest-enqueued-first.
	for _, want := range jobNames {
		mustClaimJob(t, st, want)
	}
}

func (s suite) TestStoreClaimNextJobAtomicity(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	const jobCount = 5

	for i := range jobCount {
		mustEnqueueJob(t, st, fmt.Sprintf("job-%d", i), "resource")
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

			_, jobName, found, err := st.ClaimNextJob(context.Background())
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

func (s suite) TestStoreResetStaleRunning(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustEnqueueJob(t, st, "build", "resource")
	mustClaimJob(t, st, "build")

	// Simulate a crash/interrupted run: the row is stuck "running". A fresh
	// watch startup must recover it, not leave it stranded forever.
	err := st.ResetStaleRunning(context.Background())
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	mustClaimJob(t, st, "build")
}

// TestNodeTranscriptRoundTrip covers the transcript store: absent before any
// save, returned verbatim after, and replaced (not duplicated) on a re-save
// under the same hash — the same replace-on-re-record shape nodes has.
func (s suite) TestNodeTranscriptRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	_, ok, err := st.NodeTranscript(ctx, "abc123")
	if err != nil {
		t.Fatalf("NodeTranscript (empty): %v", err)
	}

	if ok {
		t.Fatal("expected no transcript before any save")
	}

	mustRecordNode(t, st, "build", "abc123")

	err = st.SaveNodeTranscript(ctx, "abc123", `[{"type":"text","text":"hi"}]`)
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	got, ok, err := st.NodeTranscript(ctx, "abc123")
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
func (s suite) TestNodeTranscriptReplace(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := context.Background()

	mustRecordNode(t, st, "build", "abc123")

	for _, transcript := range []string{`[{"type":"text","text":"hi"}]`, `[{"type":"text","text":"replaced"}]`} {
		err := st.SaveNodeTranscript(ctx, "abc123", transcript)
		if err != nil {
			t.Fatalf("SaveNodeTranscript: %v", err)
		}
	}

	got, ok, err := st.NodeTranscript(ctx, "abc123")
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
func (s suite) TestConformanceSerialGroupsBlockAcrossJobs(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.SyncJobLimits(context.Background(), map[string][]string{
		"deploy-staging": {"deploy"},
		"deploy-prod":    {"deploy"},
		// "lint" belongs to no group at all.
	}, nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	mustEnqueueJob(t, st, "deploy-staging", "resource-a")
	mustEnqueueJob(t, st, "deploy-prod", "resource-a")
	mustEnqueueJob(t, st, "lint", "resource-a")

	id, first := mustClaimJob(t, st, "deploy-staging")

	// lint shares no group, so it is claimable while a deploy runs.
	_, third := mustClaimJob(t, st, "lint")
	if third != "lint" {
		t.Errorf("claimed %q, want lint — a job in no serial group must not be blocked by one that is", third)
	}

	// deploy-prod shares "deploy" with the running deploy-staging, so it is not.
	assertQueueEmpty(t, st)

	err = st.CompleteJob(context.Background(), id, "done", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	_, second := mustClaimJob(t, st, "deploy-prod")
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
// steps claim under test: config.Job.EffectiveMaxInFlight, Store.SyncJobLimits,
// and ClaimNextJob's admission predicate.
//
// This test replaces the guarantee TestConformanceEveryJobIsSerialRegardlessOfConfig
// used to pin, which is why that one is gone: every job being serial was the
// divergence, not the design.
func (s suite) TestConformanceMaxInFlightAdmitsUpToTheLimit(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.SyncJobLimits(context.Background(), nil, map[string]int{"build": 2})
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	// Three changes queue up. Only one row may be PENDING per job at a time,
	// so each is claimed before the next is enqueued — which is exactly how a
	// real watcher reaches two concurrent builds of one job.
	mustEnqueueJob(t, st, "build", "resource-a")
	first, _ := mustClaimJob(t, st, "build")

	mustEnqueueJob(t, st, "build", "resource-b")
	mustClaimJob(t, st, "build")

	// Two running, limit is two: the third must wait.
	mustEnqueueJob(t, st, "build", "resource-c")
	assertQueueEmpty(t, st)

	// One finishes, so a slot opens.
	err = st.CompleteJob(context.Background(), first, "done", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	mustClaimJob(t, st, "build")
}

// TestConformanceSerialForcesOneInFlight pins the precedence rule: serial:
// wins over any max_in_flight, which is why config rejects setting both.
func (s suite) TestConformanceSerialForcesOneInFlight(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	// What EffectiveMaxInFlight produces for a serial job.
	err := st.SyncJobLimits(context.Background(), nil, map[string]int{"deploy": 1})
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	mustEnqueueJob(t, st, "deploy", "resource-a")
	mustClaimJob(t, st, "deploy")

	mustEnqueueJob(t, st, "deploy", "resource-b")
	assertQueueEmpty(t, st)
}

// TestMaxInFlightDefaultsToOneForAnUnknownJob covers the row that is not
// there: a job removed from the pipeline between enqueue and claim.
//
// COALESCE defaults it to 1 rather than to unlimited, because serializing
// something nobody can describe is the conservative reading — and because the
// alternative would turn a config typo into unbounded concurrency.
func (s suite) TestMaxInFlightDefaultsToOneForAnUnknownJob(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	// Nothing synced at all.
	mustEnqueueJob(t, st, "ghost", "resource-a")
	mustClaimJob(t, st, "ghost")

	mustEnqueueJob(t, st, "ghost", "resource-b")
	assertQueueEmpty(t, st)
}

// TestConsumedMarkRoundTrip covers the get: version: every cursor: how far a
// job has fanned out, per (job, resource).
func (s suite) TestConsumedMarkRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
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

// TestSyncJobLimitsClearsBothMirrors: the tables are a declarative mirror of
// the YAML, so a max_in_flight removed from it must stop applying — the same
// property the serial-group side has, and the one a shared sync can drop by
// clearing one table and refilling both.
//
// A stale limit is the dangerous direction: 2 left behind after the field is
// deleted admits a second concurrent build of a job whose pipeline now says
// nothing about concurrency, and default-1 is what the config means by
// silence.
func (s suite) TestSyncJobLimitsClearsBothMirrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.SyncJobLimits(ctx, nil, map[string]int{"build": 2})
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	// max_in_flight: gone from the pipeline, alongside a serial group that is
	// arriving — the shape of an ordinary edit, and what makes clearing only
	// one of the two tables look like it worked.
	err = st.SyncJobLimits(ctx, map[string][]string{"lint": {"slow"}}, nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	mustEnqueueJob(t, st, "build", "resource-a")
	mustClaimJob(t, st, "build")

	mustEnqueueJob(t, st, "build", "resource-b")
	assertQueueEmpty(t, st)
}

// TestAFailedRerunUngreensTheChain: --force re-runs a chain the index holds
// as green. If that run fails, the next unforced run must not skip it on the
// strength of the older success.
func (s suite) TestAFailedRerunUngreensTheChain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustRecordJobRun(t, st, "job", "hash1", "succeeded", nil)
	mustRecordJobRun(t, st, "job", "hash1", "failed", errors.New("boom"))

	got, err := st.HasSucceededBatch(ctx, "job", []string{"hash1"})
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	if got["hash1"] {
		t.Fatal("a chain whose latest run failed is still reported as succeeded")
	}
}

// TranscriptJSON is a transcript of one text event carrying size bytes of
// text — the shape a cap test needs, exported so a driver's own footprint
// measurement can store the same thing.
func TranscriptJSON(size int) string {
	events := []map[string]string{{"type": "text", "text": strings.Repeat("t", size)}}

	encoded, err := json.Marshal(events)
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

// TestNodeTranscriptIsCapped: a transcript is the largest single value a
// driver stores, and a conversation has unboundedly many turns. Over the cap
// it is truncated, not dropped — the head is where the task and the first
// decisions are — and what remains is still JSON.
func (s suite) TestNodeTranscriptIsCapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustRecordNode(t, st, "job", "agent-node")

	err := st.SaveNodeTranscript(ctx, "agent-node", TranscriptJSON(store.MaxTranscriptBytes*3))
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	transcript, ok, err := st.NodeTranscript(ctx, "agent-node")
	if err != nil || !ok {
		t.Fatalf("NodeTranscript: ok=%v err=%v", ok, err)
	}

	if transcript == "" {
		t.Fatal("a transcript over the cap was stored as nothing at all")
	}

	if len(transcript) > store.MaxTranscriptBytes {
		t.Errorf("stored transcript is %d bytes, want at most %d", len(transcript), store.MaxTranscriptBytes)
	}

	if !json.Valid([]byte(transcript)) {
		t.Error("the truncated transcript is not valid JSON")
	}
}
