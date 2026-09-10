package storetest

import (
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

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

// TestAbortingAQueuedJobIsScopedToItsPipeline: two pipelines in one file each with a job named build.
func (s suite) TestAbortingAQueuedJobIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	mine := s.open(t, "mine")
	theirs := s.open(t, "theirs")

	mustEnqueueJob(t, mine, "build", "manual")
	mustEnqueueJob(t, theirs, "build", "manual")
	mustAbortQueued(t, mine)

	if !mustClaimNext(t, theirs) {
		t.Fatal("aborting one pipeline's queued build dropped another's")
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

	_, _, claimed, err := st.ClaimNextJob(ctxFor(t))
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	return claimed
}
