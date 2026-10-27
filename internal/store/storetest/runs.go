package storetest

// Minting, resuming and listing runs.

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestRecordRunParentIsWhatARunReports: a replay says which run it forked.
func (s suite) TestRecordRunParentIsWhatARunReports(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "build", "original", "replay")

	err := st.RecordRunParent(ctx, "replay", "original")
	if err != nil {
		t.Fatalf("RecordRunParent: %v", err)
	}

	run, found, err := st.FindRunRow(ctx, "replay")
	if err != nil || !found || run.ParentRunID != "original" {
		t.Fatalf("FindRunRow = %+v, %v, %v; want a parent of original", run, found, err)
	}
}

func mustStartRuns(ctx context.Context, t *testing.T, st store.Store, jobName string, ids ...string) {
	t.Helper()

	for _, id := range ids {
		err := st.StartRun(ctx, store.NewRun{ID: id, JobName: jobName, Workspace: "/tmp/ws"})
		if err != nil {
			t.Fatalf("StartRun(%q): %v", id, err)
		}
	}
}

func (s suite) TestListRunsHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "build", "r1", "r2", "r3")

	all, err := st.ListRuns(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListRuns(0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("ListRuns with limit 0 returned %d runs, want all 3 — zero means no limit", len(all))
	}

	two, err := st.ListRuns(ctx, "build", 2)
	if err != nil {
		t.Fatalf("ListRuns(2): %v", err)
	}

	if len(two) != 2 || two[0].ID != "r3" || two[1].ID != "r2" {
		t.Fatalf("ListRuns with limit 2 = %+v, want the newest two, newest first", two)
	}
}

// TestRunsUsingNodeHonoursItsLimit reads the same table the other way round:
// which runs touched a hash.
func (s suite) TestRunsUsingNodeHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	seedRunEvents(ctx, t, st)

	users, err := st.RunsUsingNode(ctx, "shared-hash", 0)
	if err != nil {
		t.Fatalf("RunsUsingNode(0): %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("RunsUsingNode with limit 0 returned %d runs, want both", len(users))
	}

	one, err := st.RunsUsingNode(ctx, "shared-hash", 1)
	if err != nil {
		t.Fatalf("RunsUsingNode(1): %v", err)
	}

	if len(one) != 1 {
		t.Fatalf("RunsUsingNode with limit 1 returned %d runs", len(one))
	}
}

// TestARerunRecordsWhichRunItReran: every RunRow read carries it, since the job's status and the run page both ask; and reaping the original must not take the rerun with it, because a rerun is a run in its own right.
func (s suite) TestARerunRecordsWhichRunItReran(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "ORIGINAL", "RERUN")

	err := st.RecordRunRerun(ctx, "RERUN", "ORIGINAL")
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	row, ok, err := st.FindRunRow(ctx, "RERUN")
	if err != nil || !ok || row.RerunOf != "ORIGINAL" {
		t.Fatalf("FindRunRow = %+v, %v, %v: want a rerun of ORIGINAL", row, ok, err)
	}

	runs, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	of := map[string]string{}
	for _, run := range runs {
		of[run.ID] = run.RerunOf
	}

	if of["RERUN"] != "ORIGINAL" || of["ORIGINAL"] != "" {
		t.Errorf("ListRuns reads rerun_of as %v, want only RERUN a rerun of ORIGINAL", of)
	}
}

// TestARerunOfAnOldBuildDoesNotBecomeTheJobsStatus is Concourse's rule (build.go's updateLatestCompletedBuildForJob): a rerun counts as the job's latest only when it reruns the latest build, so a green retry of last week's build cannot turn today's red job green.
func (s suite) TestARerunOfAnOldBuildDoesNotBecomeTheJobsStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "OLD", "CURRENT", "RETRYOLD")

	err := st.RecordRunRerun(ctx, "RETRYOLD", "OLD")
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	if latest := mustLatest(t, st); latest != "CURRENT" {
		t.Errorf("latest = %s, want CURRENT: a rerun of an older build became the job's status", latest)
	}

	err = st.StartRun(ctx, store.NewRun{ID: "RETRYCURRENT", JobName: "build", Workspace: "/tmp/r"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.RecordRunRerun(ctx, "RETRYCURRENT", "CURRENT")
	if err != nil {
		t.Fatalf("RecordRunRerun: %v", err)
	}

	if latest := mustLatest(t, st); latest != "RETRYCURRENT" {
		t.Errorf("latest = %s, want RETRYCURRENT: a rerun of the latest build is the job's status", latest)
	}
}

// TestLatestFinishedRunByJobLooksPastWhatIsRunning: a board colors a busy job
// by its last finished run, however many of its runs are in flight; a job with
// none finished is absent, and another pipeline's runs are never an answer.
func (s suite) TestLatestFinishedRunByJobLooksPastWhatIsRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	other := s.open(t, "other")

	mustStartRuns(ctx, t, st, "build", "B1", "B2")

	for id, status := range map[string]store.RunStatus{"B1": store.RunFailed, "B2": store.RunSucceeded} {
		err := st.FinishRun(ctx, id, status)
		if err != nil {
			t.Fatalf("FinishRun(%s): %v", id, err)
		}
	}

	// More in flight than the board's old five-run look-back reached past.
	mustStartRuns(ctx, t, st, "build", "B3", "B4", "B5", "B6", "B7", "B8")
	mustStartRuns(ctx, t, st, "lint", "L1")
	mustStartRuns(ctx, t, other, "build", "O1")

	err := other.FinishRun(ctx, "O1", store.RunAborted)
	if err != nil {
		t.Fatalf("FinishRun(O1): %v", err)
	}

	latest, err := st.LatestFinishedRunByJob(ctx)
	if err != nil {
		t.Fatalf("LatestFinishedRunByJob: %v", err)
	}

	if len(latest) != 1 || latest["build"].ID != "B2" || latest["build"].Status != store.RunSucceeded {
		t.Errorf("LatestFinishedRunByJob = %+v; want only build's B2, succeeded", latest)
	}
}

func mustLatest(t *testing.T, st interface {
	LatestRunByJob(ctx context.Context) (map[string]store.RunRow, error)
},
) string {
	t.Helper()

	latest, err := st.LatestRunByJob(context.Background())
	if err != nil {
		t.Fatalf("LatestRunByJob: %v", err)
	}

	return latest["build"].ID
}

func startRuns(t *testing.T, st interface {
	StartRun(ctx context.Context, run store.NewRun) error
}, ids ...string,
) {
	t.Helper()

	for _, id := range ids {
		err := st.StartRun(context.Background(), store.NewRun{ID: id, JobName: "build", Workspace: "/tmp/" + id})
		if err != nil {
			t.Fatalf("StartRun %s: %v", id, err)
		}
	}
}

// TestRunStepsAreKeptPerWalk is #144's key: the walk after a get counts from
// index 0 again, so a key of (run, index) kept the step before the get and
// dropped the one after it — and a resume read the first as both.
func (s suite) TestRunStepsAreKeptPerWalk(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	web := s.open(t, "web")
	infra := s.open(t, "infra")

	const run = "PERBUILD01"

	err := web.StartRun(ctx, store.NewRun{ID: run, JobName: "build", Workspace: "/tmp/web"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// The build's step first, so completion order and build-id order disagree.
	for _, step := range []store.RunStep{
		{BuildID: run + "#0", Index: 0, Name: "compile"},
		{BuildID: run, Index: 0, Name: "prep", Hash: "prephash"},
		{BuildID: run + "#0", Index: 0, Name: "compile"},
	} {
		err = web.RecordRunStep(ctx, run, step)
		if err != nil {
			t.Fatalf("RecordRunStep %+v: %v", step, err)
		}
	}

	got, err := web.CompletedRunSteps(ctx, run)
	if err != nil {
		t.Fatalf("CompletedRunSteps: %v", err)
	}

	// The hash comes back with its step: a resume chains what follows from it.
	want := []store.RunStep{
		{BuildID: run + "#0", Index: 0, Name: "compile"},
		{BuildID: run, Index: 0, Name: "prep", Hash: "prephash"},
	}

	if len(got) != len(want) {
		t.Fatalf("CompletedRunSteps = %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CompletedRunSteps = %+v, want %+v (in completion order)", got, want)
		}
	}

	other, err := infra.CompletedRunSteps(ctx, run)
	if err != nil {
		t.Fatalf("CompletedRunSteps infra: %v", err)
	}

	if len(other) != 0 {
		t.Fatalf("infra sees another pipeline's run steps: %+v", other)
	}
}

// TestStartRunRefusesAnIDSomeRunAlreadyHas is the loud half of the fix for a
// silent one.
//
// StartRun used to be an upsert on a GLOBAL primary key: a second run minting
// an id some run already held did not fail and did not insert — it took the
// existing row over, flipping a finished run back to running and repointing
// its workspace, while the row kept the OLD job_name and the OLD pipeline_id.
// Every child row of the new run then hung off a record describing a
// different run of a different job.
//
// It cannot be driven through run(): ids are random, so the collision is a
// probability rather than an input. This is the highest layer that can state
// it as a fact.
func (s suite) TestStartRunRefusesAnIDSomeRunAlreadyHas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.StartRun(ctx, store.NewRun{ID: "COLLIDE1", JobName: "build", Workspace: "/tmp/first"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.FinishRun(ctx, "COLLIDE1", store.RunSucceeded)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.StartRun(ctx, store.NewRun{ID: "COLLIDE1", JobName: "deploy", Workspace: "/tmp/second"})
	if !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("minting a colliding id returned %v, want store.ErrRunExists", err)
	}

	// And it did not take the first run over on its way to failing.
	run, ok, err := st.FindRunRow(ctx, "COLLIDE1")
	if err != nil || !ok {
		t.Fatalf("FindRunRow: %v (found %v)", err, ok)
	}

	if run.Status != store.RunSucceeded {
		t.Errorf("the first run is now %q — a refused mint still flipped it back to running", run.Status)
	}

	if run.JobName != "build" || run.Workspace != "/tmp/first" {
		t.Errorf("the first run now reads job %q workspace %q, want its own", run.JobName, run.Workspace)
	}
}

// TestResumeRunNeedsARunToResume: continuing a run is an UPDATE, so it must
// refuse to invent the row it was told to continue. The upsert could not tell
// the two apart, which is what let a mint pass as a resume.
func (s suite) TestResumeRunNeedsARunToResume(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.ResumeRun(ctx, "MISSING1", "/tmp/ws", "")
	if !errors.Is(err, store.ErrNoSuchRun) {
		t.Fatalf("resuming a run that does not exist returned %v, want store.ErrNoSuchRun", err)
	}

	err = st.StartRun(ctx, store.NewRun{ID: "REAL0001", JobName: "build", Workspace: "/tmp/first"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.FinishRun(ctx, "REAL0001", store.RunFailed)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// A resume puts the run back in flight and repoints it at the build this
	// attempt is using — which is the whole of what the upsert was doing that
	// was legitimate.
	err = st.ResumeRun(ctx, "REAL0001", "/tmp/second", "")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	run, ok, err := st.FindRunRow(ctx, "REAL0001")
	if err != nil || !ok {
		t.Fatalf("FindRunRow: %v (found %v)", err, ok)
	}

	if run.Status != store.RunRunning || run.Workspace != "/tmp/second" {
		t.Errorf("resumed run reads %q at %q, want running at the new workspace", run.Status, run.Workspace)
	}

	if run.JobName != "build" {
		t.Errorf("resuming rewrote job_name to %q", run.JobName)
	}
}
