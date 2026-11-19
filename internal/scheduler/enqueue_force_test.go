package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// claimsMidEnqueue drains a row the instant its manual enqueue lands, before Enqueue returns: the worker that wins that race.
type claimsMidEnqueue struct {
	store.Store

	drain func()
	fail  error
}

func (s *claimsMidEnqueue) EnqueueManualJob(ctx context.Context, jobName, reason, by string) error {
	if s.fail != nil {
		return s.fail
	}

	err := s.Store.EnqueueManualJob(ctx, jobName, reason, by)
	if err == nil && s.drain != nil {
		drain := s.drain
		s.drain = nil

		drain()
	}

	return err //nolint:wrapcheck // a test double passing the real store's error through
}

// A worker can claim a forced trigger's row before Enqueue returns: that run must be the forced one, and no flag may outlive it to force the poll after.
func TestAForceClaimedMidEnqueueIsSpentOnItsOwnRow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tally := filepath.Join(dir, "ran.txt")

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

	racing := &claimsMidEnqueue{Store: st}
	racing.drain = func() { runner.DrainOne(ctx, target) }
	target.Store = racing

	err := runner.Enqueue(ctx, target, "build", "trigger, no cache (web)", true)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if lines := countLines(t, tally); lines != 2 {
		t.Fatalf("tally = %d after the mid-enqueue claim, want 2: the forced row ran cached", lines)
	}

	err = st.EnqueueJob(ctx, "build", "poll", "")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	runner.DrainOne(ctx, target)

	if lines := countLines(t, tally); lines != 2 {
		t.Errorf("tally = %d, want 2: the poll after the forced run was forced by a flag its own row never spent", lines)
	}
}

// A forced trigger the store refused queued nothing, so its flag must not wait for the job's next poll.
func TestARefusedForcedEnqueueLeavesNoFlag(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tally := filepath.Join(dir, "ran.txt")

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

	target.Store = &claimsMidEnqueue{Store: st, fail: errors.New("database is locked")}

	err := runner.Enqueue(ctx, target, "build", "trigger, no cache (web)", true)
	if err == nil {
		t.Fatal("Enqueue reported success for a row the store refused")
	}

	target.Store = st

	err = st.EnqueueJob(ctx, "build", "poll", "")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	runner.DrainOne(ctx, target)

	if lines := countLines(t, tally); lines != 1 {
		t.Errorf("tally = %d, want 1: the refused trigger's flag forced the poll after it", lines)
	}
}

// cancelsBeforeFinalize stands in for a SIGINT landing after the work and before its outcome is written, recording what each finalize ran under.
type cancelsBeforeFinalize struct {
	store.Store

	cancel     context.CancelFunc
	finalizing []finalizedUnder
}

// finalizedUnder is a finalize's context as the call saw it: read later, its own cancel has already run.
type finalizedUnder struct {
	err      error
	deadline time.Time
	bounded  bool
}

func (s *cancelsBeforeFinalize) record(ctx context.Context) {
	deadline, bounded := ctx.Deadline()
	s.finalizing = append(s.finalizing, finalizedUnder{err: ctx.Err(), deadline: deadline, bounded: bounded})
}

func (s *cancelsBeforeFinalize) ClaimNextJob(ctx context.Context) (store.ClaimedJob, bool, error) {
	claim, claimed, err := s.Store.ClaimNextJob(ctx)
	if claim.JobName == "gone" {
		s.cancel()
	}

	return claim, claimed, err //nolint:wrapcheck // a test double passing the real store's error through
}

func (s *cancelsBeforeFinalize) FinishRun(ctx context.Context, runID string, status store.RunStatus) error {
	err := s.Store.FinishRun(ctx, runID, status)
	s.cancel()

	return err //nolint:wrapcheck // a test double passing the real store's error through
}

func (s *cancelsBeforeFinalize) CompleteJob(ctx context.Context, id int64, status store.QueueStatus, runErr error) error {
	s.record(ctx)

	return s.Store.CompleteJob(ctx, id, status, runErr) //nolint:wrapcheck // a test double passing the real store's error through
}

func (s *cancelsBeforeFinalize) RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, limit int) (bool, int, error) {
	s.record(ctx)

	return s.Store.RecordJobOutcome(ctx, jobName, succeeded, limit) //nolint:wrapcheck // a test double passing the real store's error through
}

// A cancelled drain still records the outcome it already has, and under a bound of its own: unbounded, a wedged database holds shutdown forever.
func TestAFinalizeUnderACancelledDrainCompletesWithinItsBound(t *testing.T) {
	t.Parallel()

	runner, target, st := drainable(t, t.TempDir(), `
jobs:
  - name: build
    plan:
      - task: pass
        inputs: []
        run: "true"
`)

	const bound = 5 * time.Second

	runner.finalizeBound = bound

	err := st.EnqueueJob(t.Context(), "gone", "a job the config no longer names", "")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	for range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		spy := &cancelsBeforeFinalize{Store: st, cancel: cancel}
		target.Store = spy

		runner.DrainOne(ctx, target)
		cancel()

		if len(spy.finalizing) == 0 {
			t.Fatal("nothing was finalized")
		}

		for _, finalize := range spy.finalizing {
			if finalize.err != nil {
				t.Errorf("a finalize ran under a cancelled context: %v", finalize.err)
			}

			if !finalize.bounded || time.Until(finalize.deadline) > bound {
				t.Errorf("finalize deadline = %v (bounded %t), want one within the runner's %s", finalize.deadline, finalize.bounded, bound)
			}
		}
	}

	if got := queueStatuses(t, st); got != "succeeded failed" {
		t.Errorf("queue = %q, want the passing build and the dropped job both finalized despite the cancel", got)
	}
}
