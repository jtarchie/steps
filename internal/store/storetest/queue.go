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

// TestAManualEnqueueIsMarkedWhicheverArrivesFirst: the breaker holds back only automatic triggers, so the claim must know a person asked — and the one-pending-row dedup must not swallow that, in either order.
func (s suite) TestAManualEnqueueIsMarkedWhicheverArrivesFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		manual []bool
		want   bool
	}{
		{"automatic only", []bool{false}, false},
		{"manual only", []bool{true}, true},
		{"automatic, then manual", []bool{false, true}, true},
		{"manual, then automatic", []bool{true, false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := s.open(t, "test")

			for _, manual := range tc.manual {
				var err error
				if manual {
					err = st.EnqueueManualJob(ctxFor(t), "build", "trigger (web)")
				} else {
					err = st.EnqueueJob(ctxFor(t), "build", "a new version")
				}

				if err != nil {
					t.Fatalf("enqueue: %v", err)
				}
			}

			id, _, claimed, err := st.ClaimNextJob(ctxFor(t))
			if err != nil || !claimed {
				t.Fatalf("ClaimNextJob = %v, %v", claimed, err)
			}

			got, err := st.QueuedTrigger(ctxFor(t), id)
			if err != nil {
				t.Fatalf("QueuedTrigger: %v", err)
			}

			if got.Manual != tc.want {
				t.Errorf("QueuedTrigger = %+v, want manual %v", got, tc.want)
			}
		})
	}
}

// TestARerunIsQueuedBesideAnOrdinaryTrigger: a retry of one build and a trigger of the job are two requests, as they are two pending builds in Concourse, so the one-pending-row dedup must not merge them; the same retry asked twice is still one row.
func (s suite) TestARerunIsQueuedBesideAnOrdinaryTrigger(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	err := st.StartRun(ctxFor(t), "ORIGINAL", "build", "/tmp/o", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	mustEnqueueJob(t, st, "build", "a new version")

	for range 2 {
		err = st.EnqueueRerunJob(ctxFor(t), "build", "retry (web)", "ORIGINAL", 1)
		if err != nil {
			t.Fatalf("EnqueueRerunJob: %v", err)
		}
	}

	rows, err := st.ListTriggerQueue(ctxFor(t), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("queue = %+v (%v), want the trigger and one retry", rows, err)
	}

	var reruns []store.QueuedTrigger

	for range 2 {
		if trigger := claimAndComplete(t, st); trigger.RerunOf != "" {
			reruns = append(reruns, trigger)
		}
	}

	if len(reruns) != 1 || reruns[0] != (store.QueuedTrigger{Manual: true, RerunOf: "ORIGINAL", RerunBuild: 1}) {
		t.Errorf("claimed reruns = %+v, want one manual rerun of ORIGINAL build 1", reruns)
	}
}

// claimAndComplete claims the next row, reads what it asks for, and finishes it, so the job's one in-flight slot frees for the next claim.
func claimAndComplete(t *testing.T, st store.Queue) store.QueuedTrigger {
	t.Helper()

	id, _, claimed, err := st.ClaimNextJob(ctxFor(t))
	if err != nil || !claimed {
		t.Fatalf("ClaimNextJob = %v, %v", claimed, err)
	}

	trigger, err := st.QueuedTrigger(ctxFor(t), id)
	if err != nil {
		t.Fatalf("QueuedTrigger: %v", err)
	}

	err = st.CompleteJob(ctxFor(t), id, "succeeded", nil)
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	return trigger
}
