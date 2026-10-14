package storetest

// A restart resumes the run a claimed row started (#208): the queue row carries the run, and the restart's recovery tells a run it interrupted from one that had already ended.

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

func (s suite) TestARestartQueuesTheRunsItInterruptedToResume(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.SyncJobLimits(ctx, nil, map[string]int{"build": 2})
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	startClaimedRun(t, st, "FIRST001")
	startClaimedRun(t, st, "SECOND01")
	mustEnqueueJob(t, st, "build", "a newer version")

	err = st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	got := []string{claimResume(t, st), claimResume(t, st)}
	if got[0] != "FIRST001" || got[1] != "SECOND01" {
		t.Errorf("claims resumed %q, want both interrupted builds, oldest first, neither collapsed into the other", got)
	}

	if next := claimAfterCompleting(t, st); next != "" {
		t.Errorf("the pending trigger came back resuming %q, want an ordinary build: it was not merged into a resume", next)
	}

	run, _, err := st.FindRunRow(ctx, "FIRST001")
	if err != nil {
		t.Fatalf("FindRunRow: %v", err)
	}

	if run.Status != store.RunAborted {
		t.Errorf("a run waiting to be resumed reads %q, want aborted until its claim puts it back in flight", run.Status)
	}
}

func (s suite) TestARestartClosesARowWhoseRunAlreadyEnded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startClaimedRun(t, st, "STOPPED1")

	// What a daemon killed between FinishRun and CompleteJob leaves: somebody's abort recorded on the run, the row still running.
	err := st.FinishRun(ctx, "STOPPED1", store.RunAborted)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	assertQueueEmpty(t, st)

	rows, err := st.ListTriggerQueue(ctx, 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	if len(rows) != 1 || rows[0].Status != "aborted" || rows[0].FinishedAt.IsZero() {
		t.Errorf("queue = %+v, want the one row closed aborted, as its run was", rows)
	}
}

func (s suite) TestARestartBeforeAResumeStartedResumesItAgain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startClaimedRun(t, st, "TWICE001")

	err := st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	// Claimed to resume, then the process died in preflight, before ResumeRun put the run back in flight.
	if got := claimResume(t, st); got != "TWICE001" {
		t.Fatalf("first restart resumed %q, want TWICE001", got)
	}

	err = st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	if got := claimResume(t, st); got != "TWICE001" {
		t.Errorf("second restart resumed %q, want TWICE001: the aborted the first restart wrote is not the run's outcome", got)
	}
}

func (s suite) TestPruneKeepsARunWaitingToBeResumed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startClaimedRun(t, st, "WAITING1")

	err := st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	// A newer pending row of the job, which the reaped run's SET NULL would collide with on the pending-job index.
	mustEnqueueJob(t, st, "build", "a newer version")
	mustStartRuns(ctx, t, st, "build", "NEWER001")

	err = st.FinishRun(ctx, "NEWER001", store.RunSucceeded)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.Prune(ctx, store.Retention{JobName: "build", Runs: 1}, "NEWER001")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got := claimResume(t, st); got != "WAITING1" {
		t.Errorf("claim resumed %q, want WAITING1: retention reaped a run its queue row was about to resume", got)
	}
}

func (s suite) TestAResumedRunIsInFlightAgain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "build", "AGAIN001")

	err := st.FinishRun(ctx, "AGAIN001", store.RunAborted)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.ResumeRun(ctx, "AGAIN001", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	run, _, err := st.FindRunRow(ctx, "AGAIN001")
	if err != nil {
		t.Fatalf("FindRunRow: %v", err)
	}

	if run.Status != store.RunRunning || !run.FinishedAt.IsZero() {
		t.Errorf("resumed run reads %q finished %v, want running with no finish time: a stale one makes its duration stop", run.Status, run.FinishedAt)
	}
}

func (s suite) TestResumableRunsListsOnlyThisPipelinesResumableRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	neighbour := s.open(t, "neighbour")

	mustStartRuns(ctx, t, st, "build", "LIVE0001", "DONE0001")
	mustStartRuns(ctx, t, neighbour, "build", "THEIRS01")
	startClaimedRun(t, st, "WAITING1")

	// A restart queued WAITING1 to resume and closed it; the next restart comes before its claim.
	err := st.ResetStaleRunning(ctx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	mustStartRuns(ctx, t, st, "build", "LIVE0002")

	err = st.FinishRun(ctx, "DONE0001", store.RunFailed)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	runs, err := st.ResumableRuns(ctx)
	if err != nil {
		t.Fatalf("ResumableRuns: %v", err)
	}

	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}

	if len(ids) != 2 || ids[0] != "WAITING1" || ids[1] != "LIVE0002" {
		t.Errorf("ResumableRuns = %q, want the run waiting to be resumed and the one still running, nothing finished and nothing of the neighbour's", ids)
	}
}

func (s suite) TestMaxStepIDIsTheHighestARunMinted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	neighbour := s.open(t, "neighbour")

	mustStartRuns(ctx, t, st, "build", "MINE0001", "EMPTY001")
	mustStartRuns(ctx, t, neighbour, "build", "THEIRS01")

	for _, row := range []struct {
		st   store.Store
		run  string
		step int64
	}{{st, "MINE0001", 3}, {st, "MINE0001", 7}, {st, "MINE0001", 5}, {neighbour, "THEIRS01", 9}} {
		err := row.st.AppendRunEvent(ctx, store.RunEventRow{RunID: row.run, Type: "step_started", StepID: row.step, At: time.Now()})
		if err != nil {
			t.Fatalf("AppendRunEvent: %v", err)
		}
	}

	for _, want := range []struct {
		run string
		max int64
	}{{"MINE0001", 7}, {"EMPTY001", 0}, {"THEIRS01", 0}} {
		got, err := st.MaxStepID(ctx, want.run)
		if err != nil {
			t.Fatalf("MaxStepID(%s): %v", want.run, err)
		}

		if got != want.max {
			t.Errorf("MaxStepID(%s) = %d, want %d", want.run, got, want.max)
		}
	}
}

// startClaimedRun enqueues and claims build, and starts runID from that claim as the scheduler does.
func startClaimedRun(t *testing.T, st store.Store, runID string) {
	t.Helper()

	mustEnqueueJob(t, st, "build", "trigger")
	id, _ := mustClaimJob(t, st, "build")

	err := st.StartRun(t.Context(), store.NewRun{ID: runID, JobName: "build", Workspace: "/tmp/ws", QueueID: id})
	if err != nil {
		t.Fatalf("StartRun(%q): %v", runID, err)
	}
}

func claimResume(t *testing.T, st store.Store) string {
	t.Helper()

	claim, found, err := st.ClaimNextJob(context.Background())
	if err != nil || !found {
		t.Fatalf("ClaimNextJob: %v (found %v)", err, found)
	}

	return claim.Resume
}

// claimAfterCompleting frees the two slots the resumes hold, then claims what is left.
func claimAfterCompleting(t *testing.T, st store.Store) string {
	t.Helper()

	ctx := t.Context()

	rows, err := st.ListTriggerQueue(ctx, 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	for _, row := range rows {
		if row.Status == "running" {
			err = st.CompleteJob(ctx, row.ID, "succeeded", nil)
			if err != nil {
				t.Fatalf("CompleteJob: %v", err)
			}
		}
	}

	return claimResume(t, st)
}
