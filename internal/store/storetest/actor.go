package storetest

// Who started a run: decided when the job is queued, carried onto the run when the run is recorded.

import (
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestAQueuedRunInheritsItsRequester: the run a queue row starts says who asked for that row, read in the statement that records the run.
func (s suite) TestAQueuedRunInheritsItsRequester(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.EnqueueJob(ctxFor(t), "build", "repo", "check:repo")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	assertStartedBy(t, st, "check:repo")
}

// TestAPersonsTriggerTakesOverAPendingRow: a click merged into a row a check left makes the row manual, and the person is why — so the run says them.
func (s suite) TestAPersonsTriggerTakesOverAPendingRow(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.EnqueueJob(ctxFor(t), "build", "repo", "check:repo")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	err = st.EnqueueManualJob(ctxFor(t), "build", "trigger (web)", "local:ops")
	if err != nil {
		t.Fatalf("EnqueueManualJob: %v", err)
	}

	assertStartedBy(t, st, "local:ops")
}

// TestARerunSaysWhoAskedForIt: a retry is a person's request, recorded on the retry rather than inherited from the run it repeats.
func (s suite) TestARerunSaysWhoAskedForIt(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.StartRun(ctxFor(t), store.NewRun{ID: "ORIGINAL", JobName: "build", Workspace: "/tmp/o", CreatedBy: "check:repo"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.EnqueueRerunJob(ctxFor(t), "build", "retry (web)", "ORIGINAL", "local:ops")
	if err != nil {
		t.Fatalf("EnqueueRerunJob: %v", err)
	}

	assertStartedBy(t, st, "local:ops")
}

// TestARunNoQueueStartedKeepsItsOwnRequester: a run from a terminal has no row to inherit from, and keeps what its caller said.
func (s suite) TestARunNoQueueStartedKeepsItsOwnRequester(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.StartRun(ctxFor(t), store.NewRun{ID: "LOCAL", JobName: "build", Workspace: "/tmp/l", CreatedBy: "os:jt"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	run, found, err := st.FindRunRow(ctxFor(t), "LOCAL")
	if err != nil || !found || run.CreatedBy != "os:jt" {
		t.Fatalf("FindRunRow = %+v, %v, %v; want created by os:jt", run, found, err)
	}
}

// assertStartedBy claims the next row, starts its run the way the scheduler does, and checks every RunRow read agrees on who started it.
func assertStartedBy(t *testing.T, st store.Store, want string) {
	t.Helper()

	claim, claimed, err := st.ClaimNextJob(ctxFor(t))
	if err != nil || !claimed {
		t.Fatalf("ClaimNextJob = %v, %v", claimed, err)
	}

	err = st.StartRun(ctxFor(t), store.NewRun{ID: "CLAIMED", JobName: claim.JobName, Workspace: "/tmp/c", QueueID: claim.ID, CreatedBy: "ignored:caller"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	run, found, err := st.FindRunRow(ctxFor(t), "CLAIMED")
	if err != nil || !found || run.CreatedBy != want {
		t.Fatalf("FindRunRow = %+v, %v, %v; want created by %q", run, found, err, want)
	}

	runs, err := st.ListRuns(ctxFor(t), claim.JobName, 10)
	if err != nil || len(runs) == 0 || runs[0].CreatedBy != want {
		t.Fatalf("ListRuns = %+v, %v; want the newest created by %q", runs, err, want)
	}
}
