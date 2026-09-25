package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func (s suite) TestListNodes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	record := store.NodeRecord{
		Hash: "node-1", ParentHash: "", Kind: "task", StepIndex: 0,
		Resource: "compile", Content: map[string]any{"run": "make"},
	}

	err := st.RecordNode(ctx, record, "build", "failed", nil, errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListNodes(ctx, "build", 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d nodes, want 1", len(rows))
	}

	row := rows[0]
	if row.Kind != "task" || row.Resource != "compile" {
		t.Errorf("got kind=%q resource=%q, want task/compile", row.Kind, row.Resource)
	}

	if row.Status != "failed" || row.Error != "boom" {
		t.Errorf("got status=%q error=%q, want failed/boom", row.Status, row.Error)
	}
}

func (s suite) TestListNodesOnAnEmptyStore(t *testing.T) {
	t.Parallel()

	rows, err := s.open(t, "test").ListNodes(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("listing an empty store should not error: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("got %d rows from an empty store, want 0", len(rows))
	}
}

func (s suite) TestListTriggerQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.EnqueueJob(ctx, "build", "resource repo changed")
	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListTriggerQueue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d queue rows, want 1", len(rows))
	}

	if rows[0].JobName != "build" || rows[0].Reason != "resource repo changed" {
		t.Errorf("got job=%q reason=%q, want build / resource repo changed", rows[0].JobName, rows[0].Reason)
	}

	if rows[0].Status != "pending" {
		t.Errorf("Status = %q, want pending", rows[0].Status)
	}

	// Never started, so those timestamps stay zero rather than being faked.
	if !rows[0].StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want zero for a pending row", rows[0].StartedAt)
	}
}

// TestAQueueRowKeepsTheInstantItWasEnqueued: the row's enqueue time is
// compared against runs.started_at to ask whether a run has started SINCE —
// the follow page and the run strip both ask it. A run that started earlier
// in the same second must not be that answer, which a whole-second stamp
// cannot promise.
func (s suite) TestAQueueRowKeepsTheInstantItWasEnqueued(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.StartRun(ctx, "run-before", "build", "", "")
	if err != nil {
		t.Fatal(err)
	}

	mustEnqueueJob(t, st, "build", "poll")

	rows, err := st.ListTriggerQueue(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListTriggerQueue: %v (%d rows)", err, len(rows))
	}

	_, found, err := st.FirstRunSince(ctx, "build", rows[0].EnqueuedAt)
	if err != nil || found {
		t.Errorf("a run started before the enqueue counts as started since it (found=%v, err=%v)", found, err)
	}
}
