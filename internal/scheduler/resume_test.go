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
