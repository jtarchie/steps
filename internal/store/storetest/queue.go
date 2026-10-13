package storetest

// The trigger queue, the breaker, and the job limits a claim honours.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestTheBreakerCountsHoldsAndResets: consecutive failures up to the limit
// hold a job, a success releases it, and no limit never holds.
func (s suite) TestTheBreakerCountsHoldsAndResets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	assertOutcome(t, st, "deploy", false, 2, false, 1)

	before := time.Now()

	assertOutcome(t, st, "deploy", false, 2, true, 2)

	held, err := st.HeldJobs(ctx)
	if err != nil || len(held) != 1 || held[0].Name != "deploy" || held[0].Consecutive != 2 {
		t.Fatalf("HeldJobs = %+v, %v; want deploy held after 2 failures", held, err)
	}

	assertStamped(t, "HeldAt", held[0].HeldAt, before, time.Now())

	assertOutcome(t, st, "deploy", true, 2, false, 0)
	assertNotHeld(t, st, "deploy")

	for failures := range 5 {
		assertOutcome(t, st, "unbounded", false, 0, false, failures+1)
	}

	assertNotHeld(t, st, "unbounded")
}

func assertOutcome(t *testing.T, st store.Store, job string, succeeded bool, maxFailures int, wantHeld bool, wantConsecutive int) {
	t.Helper()

	held, consecutive, err := st.RecordJobOutcome(context.Background(), job, succeeded, maxFailures)
	if err != nil || held != wantHeld || consecutive != wantConsecutive {
		t.Fatalf("RecordJobOutcome(%q, succeeded=%v) = held %v, %d in a row, %v; want held %v, %d in a row",
			job, succeeded, held, consecutive, err, wantHeld, wantConsecutive)
	}
}

func assertNotHeld(t *testing.T, st store.Store, job string) {
	t.Helper()

	held, err := st.IsJobHeld(context.Background(), job)
	if err != nil || held {
		t.Fatalf("IsJobHeld(%q) = %v, %v; want false", job, held, err)
	}
}

// TestSerialGroupHolderNamesWhoIsRunning: a job waiting on a lock can say
// whom it is waiting for, and nobody is named once the lock is free.
func (s suite) TestSerialGroupHolderNamesWhoIsRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.SyncJobLimits(ctx, map[string][]string{"deploy-a": {"target"}, "deploy-b": {"target"}}, nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	mustEnqueueJob(t, st, "deploy-a", "r")
	id, _ := mustClaimJob(t, st, "deploy-a")

	for job, want := range map[string]string{"deploy-b": "deploy-a", "unrelated": ""} {
		holder, err := st.SerialGroupHolder(ctx, job)
		if err != nil || holder != want {
			t.Errorf("SerialGroupHolder(%q) = %q, %v; want %q", job, holder, err, want)
		}
	}

	err = st.CompleteJob(ctx, id, "succeeded", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	holder, err := st.SerialGroupHolder(ctx, "deploy-b")
	if err != nil || holder != "" {
		t.Errorf("SerialGroupHolder after the holder finished = %q, %v; want nobody", holder, err)
	}
}

func (s suite) TestListTriggerQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.EnqueueJob(ctx, "build", "resource repo changed")
	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListTriggerQueue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d queue rows, want 1", len(rows))
	}

	if rows[0].JobName != "build" || rows[0].Reason != "resource repo changed" {
		t.Errorf("got job=%q reason=%q, want build / resource repo changed", rows[0].JobName, rows[0].Reason)
	}

	if rows[0].Status != "pending" {
		t.Errorf("Status = %q, want pending", rows[0].Status)
	}

	// Never started, so those timestamps stay zero rather than being faked.
	if !rows[0].StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want zero for a pending row", rows[0].StartedAt)
	}
}

// TestAQueueRowKeepsTheInstantItWasEnqueued: the row's enqueue time is
// compared against runs.started_at to ask whether a run has started SINCE —
// the follow page and the run strip both ask it. A run that started earlier
// in the same second must not be that answer, which a whole-second stamp
// cannot promise.
func (s suite) TestAQueueRowKeepsTheInstantItWasEnqueued(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.StartRun(ctx, store.NewRun{ID: "run-before", JobName: "build"})
	if err != nil {
		t.Fatal(err)
	}

	mustEnqueueJob(t, st, "build", "poll")

	rows, err := st.ListTriggerQueue(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListTriggerQueue: %v (%d rows)", err, len(rows))
	}

	_, found, err := st.FirstRunSince(ctx, "build", rows[0].EnqueuedAt)
	if err != nil || found {
		t.Errorf("a run started before the enqueue counts as started since it (found=%v, err=%v)", found, err)
	}
}

func (s suite) TestListTriggerQueueHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	// Three jobs: a pending row is one per job, so three of one job is one row.
	for _, job := range []string{"build", "test", "deploy"} {
		mustEnqueueJob(t, st, job, "poll")
	}

	all, err := st.ListTriggerQueue(ctx, 0)
	if err != nil {
		t.Fatalf("ListTriggerQueue(0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("ListTriggerQueue with limit 0 returned %d rows, want all 3", len(all))
	}

	two, err := st.ListTriggerQueue(ctx, 2)
	if err != nil {
		t.Fatalf("ListTriggerQueue(2): %v", err)
	}

	if len(two) != 2 {
		t.Fatalf("ListTriggerQueue with limit 2 returned %d rows", len(two))
	}
}

// TestAbortingAQueuedJobMeansItNeverRuns: Concourse's reading of an aborted pending build — it keeps its row, reads aborted, and is never claimed.
func (s suite) TestAbortingAQueuedJobMeansItNeverRuns(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustEnqueueJob(t, st, "build", "manual")

	if !mustAbortQueued(t, st) {
		t.Fatal("AbortQueuedJob found nothing to abort")
	}

	if mustClaimNext(t, st) {
		t.Fatal("an aborted row was claimed")
	}

	rows, err := st.ListTriggerQueue(ctxFor(t), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "aborted" || rows[0].FinishedAt.IsZero() {
		t.Fatalf("queue = %+v (%v), want the one row kept, aborted and finished", rows, err)
	}

	if mustAbortQueued(t, st) {
		t.Fatal("a second AbortQueuedJob found something to abort: nothing is queued")
	}
}

// TestAbortingAQueuedJobLeavesTheRunningBuildAlone: the pending slot frees for the next trigger, while a running build is stopped through its context and holds its serial slot until it ends.
func (s suite) TestAbortingAQueuedJobLeavesTheRunningBuildAlone(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	mustEnqueueJob(t, st, "build", "manual")
	mustAbortQueued(t, st)
	mustEnqueueJob(t, st, "build", "manual")

	if !mustClaimNext(t, st) {
		t.Fatal("the job's pending slot stayed taken after its queued run was aborted")
	}

	if mustAbortQueued(t, st) {
		t.Fatal("AbortQueuedJob flipped a running row")
	}
}

func mustAbortQueued(t *testing.T, st store.Queue) bool {
	t.Helper()

	dropped, err := st.AbortQueuedJob(ctxFor(t), "build")
	if err != nil {
		t.Fatalf("AbortQueuedJob: %v", err)
	}

	return dropped
}

func mustClaimNext(t *testing.T, st store.Queue) bool {
	t.Helper()

	_, claimed, err := st.ClaimNextJob(ctxFor(t))
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	return claimed
}

// TestAManualEnqueueIsMarkedWhicheverArrivesFirst: the breaker holds back only automatic triggers, so the claim must know a person asked — and the one-pending-row dedup must not swallow that, in either order.
func (s suite) TestAManualEnqueueIsMarkedWhicheverArrivesFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		manual []bool
		want   bool
	}{
		{"automatic only", []bool{false}, false},
		{"manual only", []bool{true}, true},
		{"automatic, then manual", []bool{false, true}, true},
		{"manual, then automatic", []bool{true, false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := s.open(t, "test")

			for _, manual := range tc.manual {
				var err error
				if manual {
					err = st.EnqueueManualJob(ctxFor(t), "build", "trigger (web)")
				} else {
					err = st.EnqueueJob(ctxFor(t), "build", "a new version")
				}

				if err != nil {
					t.Fatalf("enqueue: %v", err)
				}
			}

			claim, claimed, err := st.ClaimNextJob(ctxFor(t))
			if err != nil || !claimed {
				t.Fatalf("ClaimNextJob = %v, %v", claimed, err)
			}

			if claim.Manual != tc.want {
				t.Errorf("ClaimNextJob = %+v, want manual %v", claim, tc.want)
			}
		})
	}
}

// TestARerunIsQueuedBesideAnOrdinaryTrigger: a retry of one build and a trigger of the job are two requests, as they are two pending builds in Concourse, so the one-pending-row dedup must not merge them; the same retry asked twice is still one row.
func (s suite) TestARerunIsQueuedBesideAnOrdinaryTrigger(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.StartRun(ctxFor(t), store.NewRun{ID: "ORIGINAL", JobName: "build", Workspace: "/tmp/o"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	mustEnqueueJob(t, st, "build", "a new version")

	for range 2 {
		err = st.EnqueueRerunJob(ctxFor(t), "build", "retry (web)", "ORIGINAL")
		if err != nil {
			t.Fatalf("EnqueueRerunJob: %v", err)
		}
	}

	rows, err := st.ListTriggerQueue(ctxFor(t), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("queue = %+v (%v), want the trigger and one retry", rows, err)
	}

	var reruns []store.ClaimedJob

	for range 2 {
		if claim := claimAndComplete(t, st); claim.RerunOf != "" {
			reruns = append(reruns, claim)
		}
	}

	if len(reruns) != 1 || reruns[0] != (store.ClaimedJob{ID: reruns[0].ID, JobName: "build", Manual: true, RerunOf: "ORIGINAL"}) {
		t.Errorf("claimed reruns = %+v, want one manual rerun of all of ORIGINAL", reruns)
	}
}

// claimAndComplete claims the next row and finishes it, so the job's one in-flight slot frees for the next claim.
func claimAndComplete(t *testing.T, st store.Queue) store.ClaimedJob {
	t.Helper()

	claim, claimed, err := st.ClaimNextJob(ctxFor(t))
	if err != nil || !claimed {
		t.Fatalf("ClaimNextJob = %v, %v", claimed, err)
	}

	err = st.CompleteJob(ctxFor(t), claim.ID, "succeeded", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	return claim
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

	claim, found, err := st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if !found {
		t.Fatal("ClaimNextJob: expected a pending job, queue was empty")
	}

	if want != "" && claim.JobName != want {
		t.Fatalf("ClaimNextJob = %q, want %q", claim.JobName, want)
	}

	return claim.ID, claim.JobName
}

func assertQueueEmpty(t *testing.T, st store.Store) {
	t.Helper()

	_, found, err := st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if found {
		t.Fatal("expected the queue to be empty")
	}
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

			claim, found, err := st.ClaimNextJob(context.Background())
			jobName := claim.JobName
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

// TestStoreResetStaleRunningClosesRuns: the queue row is re-queued, but the
// run that held it is over — its process is gone — and a row left "running"
// keeps a live spinner on it forever. Scoped like the queue: another
// pipeline's run in the same state file may really be in flight.
func (s suite) TestStoreResetStaleRunningClosesRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	neighbour := s.open(t, "neighbour")

	mustStartRuns(ctx, t, st, "build", "STALE001", "DONE0001")
	mustStartRuns(ctx, t, neighbour, "build", "OTHER001")

	err := st.FinishRun(ctx, "DONE0001", store.RunSucceeded)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	for _, want := range []struct {
		st     store.Store
		id     string
		status store.RunStatus
	}{{st, "STALE001", store.RunAborted}, {st, "DONE0001", store.RunSucceeded}, {neighbour, "OTHER001", store.RunRunning}} {
		run, ok, err := want.st.FindRunRow(ctx, want.id)
		if err != nil || !ok {
			t.Fatalf("FindRunRow %s: %v (found %v)", want.id, err, ok)
		}

		if run.Status != want.status {
			t.Errorf("run %s is %q, want %q", want.id, run.Status, want.status)
		}

		if want.status == store.RunAborted && run.FinishedAt.IsZero() {
			t.Errorf("run %s was closed with no finish time", want.id)
		}
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
