package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store"
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

func TestListNodes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := openTestStore(t)

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

func TestListNodesOnAnEmptyStore(t *testing.T) {
	t.Parallel()

	rows, err := openTestStore(t).ListNodes(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("listing an empty st should not error: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("got %d rows from an empty st, want 0", len(rows))
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
