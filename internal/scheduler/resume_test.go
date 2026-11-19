package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// A restart resumes in the tree the run left, so a tree that went (a temp dir the OS cleaned, a root somebody emptied) leaves nothing to continue in: the job is built from the top as a new run, what every restart did before there was a resume, rather than skipping steps whose files are gone.
func TestAResumeWhoseTreeIsGoneBuildsFromTheTop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tally := filepath.Join(dir, "tally")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: first
        inputs: []
        run: echo first >> `+tally+`
      - task: second
        inputs: []
        run: echo second >> `+tally+`
`)

	ctx := context.Background()

	interruptRun(t, st, "GONETREE", filepath.Join(dir, "gone"))

	PrepareQueue(ctx, target)
	runner.DrainOne(ctx, target)

	if got := queueStatuses(t, st); got != "succeeded" {
		t.Fatalf("queue = %s, want the job built", got)
	}

	if got := tallied(t, tally); len(got) != 2 || got[0] != "first" {
		t.Errorf("tally = %q, want both steps run: the finished one's files went with the tree", got)
	}

	runs, err := st.ListRuns(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 2 || runs[0].ID == "GONETREE" || runs[1].Status != store.RunAborted {
		t.Errorf("runs = %+v, want a new run beside the interrupted one, which stays aborted", runs)
	}
}

// A person's resume whose tree went between the press and the claim fails rather than falling back as a restart's does: building from the top is the cost they pressed Resume to avoid. Nothing ran, so the breaker does not count it.
func TestAPersonsResumeWhoseTreeIsGoneFailsRatherThanRebuilding(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tally := filepath.Join(dir, "tally")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    max_consecutive_failures: 1
    plan:
      - task: first
        inputs: []
        run: echo first >> `+tally+`
`)

	ctx := context.Background()

	interruptRun(t, st, "ASKED001", filepath.Join(dir, "gone"))

	err := st.FinishRun(ctx, "ASKED001", store.RunFailed)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.CompleteJob(ctx, 1, store.QueueFailed, nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	err = st.EnqueueResumeJob(ctx, "build", "resume (web)", "ASKED001", "")
	if err != nil {
		t.Fatalf("EnqueueResumeJob: %v", err)
	}

	runner.DrainOne(ctx, target)

	if got := queueStatuses(t, st); !strings.HasSuffix(got, "failed") {
		t.Errorf("queue = %s, want the resume failed", got)
	}

	_, err = os.Stat(tally)
	if !os.IsNotExist(err) {
		t.Errorf("a step ran (%v): the refused resume built from the top", err)
	}

	runs, err := st.ListRuns(ctx, "build", 0)
	if err != nil || len(runs) != 1 || runs[0].Status != store.RunFailed {
		t.Errorf("runs = %+v (%v), want the one run, still failed", runs, err)
	}

	requireNotHeld(t, st, "a refused resume ran nothing for the breaker to count")
}

func requireNotHeld(t *testing.T, st store.Store, why string) {
	t.Helper()

	held, err := st.IsJobHeld(t.Context(), "build")
	if err != nil || held {
		t.Errorf("IsJobHeld = %v (%v), want false: %s", held, err, why)
	}
}

// A resume claimed ahead of a "no cache" trigger of the same job must not spend the flag that trigger set: the resume is not the job's ordinary row.
func TestAResumeClaimLeavesAnotherRequestsForceAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tally := filepath.Join(dir, "tally")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: append
        inputs: []
        run: echo ran >> `+tally+`
`)
	ctx := t.Context()

	runner.DrainOne(ctx, target)

	runs, err := st.ListRuns(ctx, "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %+v (%v), want the one run", runs, err)
	}

	err = st.FinishRun(ctx, runs[0].ID, store.RunFailed)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.EnqueueResumeJob(ctx, "build", "resume (web)", runs[0].ID, "")
	if err != nil {
		t.Fatalf("EnqueueResumeJob: %v", err)
	}

	_, err = runner.Enqueue(ctx, target, "build", "trigger, no cache (cli)", true)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	runner.DrainOne(ctx, target)
	runner.DrainOne(ctx, target)

	if lines := countLines(t, tally); lines != 2 {
		t.Errorf("tally = %d, want 2: the no-cache trigger lost its flag to the resume claimed before it", lines)
	}
}

// interruptRun is what a killed daemon leaves: a claimed row, its run started in tree with the first step done.
func interruptRun(t *testing.T, st store.Store, runID, tree string) {
	t.Helper()

	claim, found, err := st.ClaimNextJob(t.Context())
	if err != nil || !found {
		t.Fatalf("ClaimNextJob: %v (found %v)", err, found)
	}

	err = st.StartRun(t.Context(), store.NewRun{ID: runID, JobName: "build", Workspace: tree, QueueID: claim.ID})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.RecordRunStep(t.Context(), runID, store.RunStep{BuildID: runID, Index: 0, Name: "first"})
	if err != nil {
		t.Fatalf("RecordRunStep: %v", err)
	}
}

func tallied(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir() file the test's own pipeline wrote
	if err != nil {
		t.Fatalf("read tally: %v", err)
	}

	return strings.Fields(string(data))
}

// Somebody's abort landing as the daemon goes down is still an abort: the run is closed, never left for the restart to resume.
func TestAnAbortDuringShutdownIsStillFinal(t *testing.T) {
	t.Parallel()

	runner, target, st := slowPipeline(t)
	drain, shutdown := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.DrainOne(drain, target)
	}()

	runID := runningRunID(t, st)

	if !runner.Abort(target, runID) {
		t.Fatal("Abort could not find the run this runner is executing")
	}

	shutdown()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the aborted run never returned")
	}

	assertRunStatus(t, st, runID, store.RunAborted)
}

// A build waited for through a shutdown that then fails on its own has an outcome, not an interruption: closed failed like its queue row, never left running for a restart to retry.
func TestABuildThatFailsInTheShutdownGraceIsFailed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: deploy
        inputs: []
        run: |
          touch `+started+`
          sleep 1
          exit 1
`)

	process, shutdown := context.WithCancel(t.Context())
	defer shutdown()

	runner.StopWith(process)

	drain, stop := context.WithCancel(process)
	defer stop()

	done := drainInBackground(drain, runner, target)

	waitForFile(t, started)
	shutdown()
	waitForDrain(t, done, 20*time.Second)

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	if got := queueStatuses(t, st); got != "failed" || runs[0].Status != store.RunFailed {
		t.Errorf("queue = %q, run = %q, want both failed", got, runs[0].Status)
	}
}

func assertRunStatus(t *testing.T, st store.Runs, runID string, want store.RunStatus) {
	t.Helper()

	run, ok, err := st.FindRunRow(t.Context(), runID)
	if err != nil || !ok {
		t.Fatalf("FindRunRow: %v (found %v)", err, ok)
	}

	if run.Status != want {
		t.Errorf("run %s reads %q, want %q", runID, run.Status, want)
	}
}
