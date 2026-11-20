package storetest

import (
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// recordedUnder is what the read side still reports for a job: its runs, its cached nodes and its queue rows.
func recordedUnder(t *testing.T, st store.Store, job string) (runs, nodes, queued int) {
	t.Helper()

	ctx := ctxFor(t)

	listed, err := st.ListRuns(ctx, job, 0)
	if err != nil {
		t.Fatalf("ListRuns(%q): %v", job, err)
	}

	cached, err := st.ListNodes(ctx, job, 0)
	if err != nil {
		t.Fatalf("ListNodes(%q): %v", job, err)
	}

	rows, err := st.ListTriggerQueue(ctx, 0)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	for _, row := range rows {
		if row.JobName == job {
			queued++
		}
	}

	return len(listed), len(cached), queued
}

func seedFinishedJob(t *testing.T, st store.Store, job, runID string) {
	t.Helper()

	ctx := ctxFor(t)

	seedRuns(t, st, job, runID)
	must(t, "FinishRun", st.FinishRun(ctx, runID, store.RunSucceeded))
	mustRecordChainSucceeded(t, st, job, "hash-of-"+job)
	must(t, "EnqueueJob", st.EnqueueJob(ctx, job, "seeded", ""))
}

// TestRetireAbsentTakesAnUndeclaredJobsHistory: the job the configuration stopped declaring loses everything recorded under it, and neither the job still declared nor a same-named job of another pipeline in the database loses anything.
func (s suite) TestRetireAbsentTakesAnUndeclaredJobsHistory(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")
	bystander := s.open(t, "other")

	seedFinishedJob(t, st, "drop", "run-drop")
	seedFinishedJob(t, st, "keep", "run-keep")
	seedFinishedJob(t, bystander, "drop", "run-theirs")

	must(t, "RetireAbsent", st.RetireAbsent(ctx, []string{"keep"}, nil))

	if runs, nodes, queued := recordedUnder(t, st, "drop"); runs+nodes+queued != 0 {
		t.Errorf("the undeclared job still has %d runs, %d nodes and %d queue rows, want none", runs, nodes, queued)
	}

	green, err := st.HasSucceededBatch(ctx, "drop", []string{"hash-of-drop"})
	if err != nil {
		t.Fatalf("HasSucceededBatch: %v", err)
	}

	if green["hash-of-drop"] {
		t.Error("the undeclared job's chain still reads as green: a job given the name again would skip work it never did")
	}

	if runs, nodes, queued := recordedUnder(t, st, "keep"); runs != 1 || nodes != 1 || queued != 1 {
		t.Errorf("the declared job has %d runs, %d nodes and %d queue rows, want one of each", runs, nodes, queued)
	}

	if runs, nodes, queued := recordedUnder(t, bystander, "drop"); runs != 1 || nodes != 1 || queued != 1 {
		t.Errorf("another pipeline's job of the same name has %d runs, %d nodes and %d queue rows, want one of each", runs, nodes, queued)
	}
}

// resourceHistory is how many versions a resource has recorded and how far a job has consumed it.
func resourceHistory(t *testing.T, st store.Store, job, resource string) (versions, mark int) {
	t.Helper()

	recorded, err := st.VersionOrders(ctxFor(t), resource)
	if err != nil {
		t.Fatalf("VersionOrders(%q): %v", resource, err)
	}

	consumed, err := st.ConsumedMark(ctxFor(t), job, resource)
	if err != nil {
		t.Fatalf("ConsumedMark(%q): %v", resource, err)
	}

	return len(recorded), int(consumed)
}

// TestRetireAbsentTakesAnUndeclaredResourcesHistory: versions, the check cursor, the check error and the passed: records go with the resource; the resource still declared keeps its own.
func (s suite) TestRetireAbsentTakesAnUndeclaredResourcesHistory(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")
	version := []map[string]any{{"ref": "v1"}}

	for _, resource := range []string{"gone", "repo"} {
		_, err := st.RecordVersions(ctx, resource, version, 0)
		if err != nil {
			t.Fatalf("RecordVersions(%q): %v", resource, err)
		}

		must(t, "RecordCheckError", st.RecordCheckError(ctx, resource, "unreachable"))
		must(t, "RecordConsumedMark", st.RecordConsumedMark(ctx, "build", resource, 1))
	}

	must(t, "RetireAbsent", st.RetireAbsent(ctx, []string{"build"}, []string{"repo"}))

	for resource, want := range map[string]int{"gone": 0, "repo": 1} {
		versions, mark := resourceHistory(t, st, "build", resource)
		if versions != want || mark != want {
			t.Errorf("%s has %d versions and build's mark on it is %d after the retire, want %d of each", resource, versions, mark, want)
		}
	}

	failing, err := st.CheckErrors(ctx)
	if err != nil {
		t.Fatalf("CheckErrors: %v", err)
	}

	if len(failing) != 1 || failing[0].Name != "repo" {
		t.Errorf("check errors after the retire = %+v, want only repo's", failing)
	}
}

// TestRetireAbsentWithNothingDeclaredTakesEverything: an empty list is "declares none", never "no opinion" — a NULL list compares as unknown in both dialects and would delete nothing.
func (s suite) TestRetireAbsentWithNothingDeclaredTakesEverything(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	seedFinishedJob(t, st, "build", "run-one")

	_, err := st.RecordVersions(ctx, "repo", []map[string]any{{"ref": "v1"}}, 0)
	if err != nil {
		t.Fatalf("RecordVersions: %v", err)
	}

	must(t, "RetireAbsent", st.RetireAbsent(ctx, nil, nil))

	if runs, nodes, queued := recordedUnder(t, st, "build"); runs+nodes+queued != 0 {
		t.Errorf("with no job declared, build still has %d runs, %d nodes and %d queue rows", runs, nodes, queued)
	}

	versions, err := st.VersionOrders(ctx, "repo")
	if err != nil {
		t.Fatalf("VersionOrders: %v", err)
	}

	if len(versions) != 0 {
		t.Errorf("with no resource declared, repo still has %d versions", len(versions))
	}
}

// TestARunningBuildPutsItsJobsRetirementOff: the build writes under its run to the end, so the job goes when the build does — at the prune that ends it — and not before.
func (s suite) TestARunningBuildPutsItsJobsRetirementOff(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	seedRuns(t, st, "drop", "run-live")

	must(t, "RetireAbsent", st.RetireAbsent(ctx, nil, nil))

	if runs, _, _ := recordedUnder(t, st, "drop"); runs != 1 {
		t.Fatalf("a job with a build running has %d runs after the retire, want its running one", runs)
	}

	// What the build's last writes need: the run still there to hang them on.
	must(t, "RecordRunStep", st.RecordRunStep(ctx, "run-live", store.RunStep{BuildID: "run-live", Visit: 1, Name: "compile"}))
	must(t, "FinishRun", st.FinishRun(ctx, "run-live", store.RunSucceeded))
	must(t, "Prune", st.Prune(ctx, store.Retention{JobName: "drop", Runs: 10}, "run-live"))

	if runs, _, _ := recordedUnder(t, st, "drop"); runs != 0 {
		t.Errorf("the retired job still has %d runs after its build ended and pruned, want none", runs)
	}
}

// TestCompletingARetiredJobsQueueRowTakesTheJob: a daemon's build ends at CompleteJob, after its prune, so that is where its retirement lands.
func (s suite) TestCompletingARetiredJobsQueueRowTakesTheJob(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	must(t, "EnqueueJob", st.EnqueueJob(ctx, "drop", "seeded", ""))

	claimed, ok, err := st.ClaimNextJob(ctx)
	if err != nil || !ok {
		t.Fatalf("ClaimNextJob = %v, %v, want the row", ok, err)
	}

	must(t, "RetireAbsent", st.RetireAbsent(ctx, nil, nil))

	if _, _, queued := recordedUnder(t, st, "drop"); queued != 1 {
		t.Fatalf("a job with a claimed row has %d queue rows after the retire, want its running one", queued)
	}

	must(t, "CompleteJob", st.CompleteJob(ctx, claimed.ID, store.QueueSucceeded, nil))

	if _, _, queued := recordedUnder(t, st, "drop"); queued != 0 {
		t.Errorf("the retired job still has %d queue rows after its build completed, want none", queued)
	}
}

// TestAJobDeclaredAgainBeforeItGoesIsKept: a set that removed a job and a set that put it back, both while its build ran, leave it a job.
func (s suite) TestAJobDeclaredAgainBeforeItGoesIsKept(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	seedRuns(t, st, "build", "run-live")

	must(t, "RetireAbsent without it", st.RetireAbsent(ctx, nil, nil))
	must(t, "RetireAbsent with it", st.RetireAbsent(ctx, []string{"build"}, nil))
	must(t, "FinishRun", st.FinishRun(ctx, "run-live", store.RunSucceeded))
	must(t, "Prune", st.Prune(ctx, store.Retention{JobName: "build", Runs: 10}, "run-live"))

	if runs, _, _ := recordedUnder(t, st, "build"); runs != 1 {
		t.Errorf("a job declared again has %d runs after its build ended, want the one it recorded", runs)
	}
}
