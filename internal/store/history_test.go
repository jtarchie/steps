package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// openTestStore returns a Store backed by a fresh temp database.
func openTestStore(t *testing.T) *Store {
	t.Helper()

	st, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func TestListJobRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openTestStore(t)

	mustRecordNode(t, st, "build", "hash-1")
	mustRecordNode(t, st, "deploy", "hash-2")

	err := st.RecordJobRun(ctx, "build", "hash-1", "succeeded", nil)
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordJobRun(ctx, "deploy", "hash-2", "failed", errors.New("exit status 1"))
	if err != nil {
		t.Fatal(err)
	}

	all, err := st.ListJobRuns(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(all) != 2 {
		t.Fatalf("got %d runs, want 2", len(all))
	}

	// An empty job name means every job; a named one filters.
	only, err := st.ListJobRuns(ctx, "deploy", 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(only) != 1 {
		t.Fatalf("got %d runs for deploy, want 1", len(only))
	}

	if only[0].Status != "failed" {
		t.Errorf("Status = %q, want failed", only[0].Status)
	}

	// The error text is what makes the row worth reading back.
	if only[0].Error != "exit status 1" {
		t.Errorf("Error = %q, want the recorded failure text", only[0].Error)
	}

	if only[0].CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want the recorded timestamp")
	}
}

func TestListJobRunsRespectsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openTestStore(t)

	for _, hash := range []string{"a", "b", "c"} {
		mustRecordNode(t, st, "build", hash)

		err := st.RecordJobRun(ctx, "build", hash, "succeeded", nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.ListJobRuns(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Errorf("got %d rows, want the limit of 2", len(rows))
	}
}

func TestListNodes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openTestStore(t)

	record := NodeRecord{
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

func TestListNodesOnAnEmptyStore(t *testing.T) {
	t.Parallel()

	rows, err := openTestStore(t).ListNodes(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("listing an empty store should not error: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("got %d rows from an empty store, want 0", len(rows))
	}
}

func TestListTriggerQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openTestStore(t)

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
