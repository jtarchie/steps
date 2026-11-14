package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// A shutdown nobody asked the run to stop for is an interruption, not an abort: the run is left running for the next start to resume (#208).
func TestAShutdownLeavesAnInterruptedRunRunningForTheRestart(t *testing.T) {
	t.Parallel()

	runner, target, st := slowPipeline(t)
	drain, shutdown := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.DrainOne(drain, target)
	}()

	runID := runningRunID(t, st)

	shutdown()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the interrupted run never returned")
	}

	assertRunStatus(t, st, runID, store.RunRunning)
}

// A restart resumes the run it interrupted in the tree that run left: the same run id, continued to its end, and no second run beside it.
func TestAResumeInTheTreeItLeftContinuesTheSameRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree := t.TempDir()

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: first
        inputs: []
        run: "true"
      - task: second
        inputs: []
        run: "true"
`)

	ctx := context.Background()

	interruptRun(t, st, "KEPTTREE", tree)

	PrepareQueue(ctx, target)
	runner.DrainOne(ctx, target)

	if got := queueStatuses(t, st); got != "succeeded" {
		t.Fatalf("queue = %s, want the job built", got)
	}

	runs, err := st.ListRuns(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "KEPTTREE" || runs[0].Status != store.RunSucceeded {
		t.Errorf("runs = %+v, want the one interrupted run continued to success, not a new run beside it", runs)
	}
}

// --max-runs: a positive limit sets the capacity, and zero or less is no limit, which still lets a build run.
func TestLimitRunsSetsTheCapacityAndZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	runner, target, st := drainable(t, dir, `
jobs:
  - name: build
    plan:
      - task: only
        inputs: []
        run: "true"
`)

	runner.LimitRuns(3)

	if capacity, active := runner.RunCapacity(); capacity != 3 || active != 0 {
		t.Errorf("RunCapacity after LimitRuns(3) = %d, %d; want 3, 0", capacity, active)
	}

	unlimited, _, _ := drainable(t, t.TempDir(), `
jobs:
  - name: build
    plan:
      - task: only
        inputs: []
        run: "true"
`)

	for _, n := range []int{0, -1} {
		unlimited.LimitRuns(n)

		if capacity, _ := unlimited.RunCapacity(); capacity != 0 {
			t.Errorf("RunCapacity after LimitRuns(%d) = %d, want 0 (no limit)", n, capacity)
		}
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		unlimited.DrainOne(context.Background(), target)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a drain under LimitRuns(0) never took a slot")
	}

	_ = st
}

// A store that fails surfaces as an error from the person's retry and abort, wrapped with the scheduler's own prefix.
func TestRerunAndAbortQueuedSurfaceAStoreFailure(t *testing.T) {
	t.Parallel()

	runner, target, st := drainable(t, t.TempDir(), `
jobs:
  - name: build
    plan:
      - task: only
        inputs: []
        run: "true"
`)

	_ = st.Close()

	err := runner.EnqueueRerun(context.Background(), target, "build", "retry (web)", "RUN1")
	if err == nil || !strings.HasPrefix(err.Error(), "scheduler: ") {
		t.Errorf("EnqueueRerun on a closed store = %v, want a scheduler-prefixed error", err)
	}

	dropped, err := runner.AbortQueued(context.Background(), target, "build")
	if dropped || err == nil || !strings.HasPrefix(err.Error(), "scheduler: ") {
		t.Errorf("AbortQueued on a closed store = %v, %v; want false and a scheduler-prefixed error", dropped, err)
	}
}
