package trigger

// What a triggered run RECORDS when it ends — the half of drainOne that is
// not about whether the job ran.
//
// Both assertions here were unkilled mutants: the queue row's status could be
// changed from "failed" to "done" and the existing tests still passed,
// because they assert the row is FINISHED (nothing claimable) rather than
// what it finished as; and the circuit breaker's wiring had no test at all at
// this level — the breaker's own arithmetic is covered in breaker_test.go
// against the store, but nothing carried a real run's outcome into it. That
// is the one-shot path (`steps web --once`), which is what a cron line runs.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// outcomeFixture is a one-job pipeline whose task exits with `code`, plus the
// breaker limit to run it under. Everything else is the smallest pipeline
// drainOne will accept.
func outcomeFixture(t *testing.T, code, maxFailures int) (*config.Config, *store.Store, workspace.Provider) {
	t.Helper()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  max_consecutive_failures: %d
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: exit %d
`, versionsPath, maxFailures, code))

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	return cfg, st, provider
}

// queueStatus is what the row this drain finished says it finished as.
func queueStatus(t *testing.T, st *store.Store, jobName string) string {
	t.Helper()

	rows, err := st.ListTriggerQueue(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	for _, row := range rows {
		if row.JobName == jobName {
			return row.Status
		}
	}

	t.Fatalf("no queue row for job %q", jobName)

	return ""
}

// TestDrainOneRecordsAFailureAsFailed.
//
// The existing coverage asserts a failed run leaves nothing claimable, which
// is true of any terminal status — so recording a failure as "done" passed
// every test. The row IS the history: the web UI reads it, and a queue full
// of green rows for builds that failed is worse than no history at all.
func TestDrainOneRecordsAFailureAsFailed(t *testing.T) {
	t.Parallel()

	cfg, st, provider := outcomeFixture(t, 1, 0)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	ran, err := drainOne(ctx, cfg, provider, st, nil, false)
	if !ran || err == nil {
		t.Fatalf("drainOne: ran=%v err=%v, want a failing job to run and report", ran, err)
	}

	if status := queueStatus(t, st, "build"); status != "failed" {
		t.Errorf("queue row says %q, want failed", status)
	}
}

// TestDrainOneRecordsASuccessAsDone is the other side of the same column: a
// mutant that recorded everything as failed would otherwise be as invisible.
func TestDrainOneRecordsASuccessAsDone(t *testing.T) {
	t.Parallel()

	cfg, st, provider := outcomeFixture(t, 0, 0)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	ran, err := drainOne(ctx, cfg, provider, st, nil, false)
	if !ran || err != nil {
		t.Fatalf("drainOne: ran=%v err=%v, want a passing job to run cleanly", ran, err)
	}

	if status := queueStatus(t, st, "build"); status != "done" {
		t.Errorf("queue row says %q, want done", status)
	}
}

// TestDrainOneAdvancesTheCircuitBreaker carries a real run's outcome into the
// breaker.
//
// breaker_test.go covers the counting itself, against the store. What had no
// test is the WIRING — that a triggered build's failure is what advances that
// count — so every mutation of the call and its guard survived. This is the
// one-shot path a cron line drives, and the feature exists to stop a broken
// job burning model spend on every new version.
func TestDrainOneAdvancesTheCircuitBreaker(t *testing.T) {
	t.Parallel()

	cfg, st, provider := outcomeFixture(t, 1, 1)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, err = drainOne(ctx, cfg, provider, st, nil, false)
	if err == nil {
		t.Fatal("drainOne: want the failing job to report an error")
	}

	paused, err := st.IsJobPaused(ctx, "build")
	if err != nil {
		t.Fatalf("IsJobPaused: %v", err)
	}

	if !paused {
		t.Error("one failure against max_consecutive_failures: 1 did not pause the job")
	}
}

// TestDrainOneLeavesAPassingJobRunning: the breaker is a response to failure,
// and a successful build must not trip it however low the limit is set. It
// pins the other half of the guard the mutants walked through.
func TestDrainOneLeavesAPassingJobRunning(t *testing.T) {
	t.Parallel()

	cfg, st, provider := outcomeFixture(t, 0, 1)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	_, err = drainOne(ctx, cfg, provider, st, nil, false)
	if err != nil {
		t.Fatalf("drainOne: %v", err)
	}

	paused, err := st.IsJobPaused(ctx, "build")
	if err != nil {
		t.Fatalf("IsJobPaused: %v", err)
	}

	if paused {
		t.Error("a successful build paused the job")
	}
}

// TestDrainOneSaysWhenTheBreakerTrips.
//
// The line is the feature. A paused job is otherwise invisible — the poller
// stops triggering it and nothing else changes — so the message naming the
// job and the command that resumes it is the only thing standing between a
// stopped nightly build and somebody noticing next week. It also has to name
// the pipeline, since `steps jobs resume` takes one.
//
// Not t.Parallel(): captureStdout swaps this package's output writer.
func TestDrainOneSaysWhenTheBreakerTrips(t *testing.T) {
	cfg, st, provider := outcomeFixture(t, 1, 1)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	out := captureStdout(t, func() {
		_, drainErr := drainOne(ctx, cfg, provider, st, nil, false)
		if drainErr == nil {
			t.Error("drainOne: want the failing job to report an error")
		}
	})

	for _, want := range []string{"PAUSED", "build", "steps jobs resume"} {
		if !strings.Contains(out, want) {
			t.Errorf("the breaker tripped without saying %q:\n%s", want, out)
		}
	}
}

// TestDrainOneCountsTowardsThePauseOutLoud is the same message one failure
// earlier: a job on its way to being paused says how far along it is, which
// is what makes the pause itself unsurprising.
//
// Not t.Parallel(): captureStdout swaps this package's output writer.
func TestDrainOneCountsTowardsThePauseOutLoud(t *testing.T) {
	cfg, st, provider := outcomeFixture(t, 1, 3)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	out := captureStdout(t, func() {
		_, drainErr := drainOne(ctx, cfg, provider, st, nil, false)
		if drainErr == nil {
			t.Error("drainOne: want the failing job to report an error")
		}
	})

	if !strings.Contains(out, "1/3 consecutive") {
		t.Errorf("a failure short of the limit did not report the count:\n%s", out)
	}
}

// TestDrainOneSaysNothingWhenTheBreakerIsOff.
//
// max_consecutive_failures: 0 is the default and means the breaker is off, so
// a failing job must not report progress towards a limit that does not exist.
// The guard is a boundary — `<= 0` — and a mutant that moved it to `< 0`
// printed "failed (1/0 consecutive)" at every failure, which reads as a job
// one failure from a pause it will never reach.
//
// Not t.Parallel(): captureStdout swaps this package's output writer.
func TestDrainOneSaysNothingWhenTheBreakerIsOff(t *testing.T) {
	cfg, st, provider := outcomeFixture(t, 1, 0)
	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	out := captureStdout(t, func() {
		_, drainErr := drainOne(ctx, cfg, provider, st, nil, false)
		if drainErr == nil {
			t.Error("drainOne: want the failing job to report an error")
		}
	})

	if strings.Contains(out, "consecutive") {
		t.Errorf("a job with the breaker off reported progress towards a pause:\n%s", out)
	}
}
