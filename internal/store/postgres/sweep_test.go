package postgres

import (
	"fmt"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestNoContentOutlivesTheNodesThatNamedIt: node_content is interned across pipelines and nothing cascades into it, so the sweep after a node prune and after a pipeline delete is the only thing that ever removes a row.
func TestNoContentOutlivesTheNodesThatNamedIt(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	st := openStore(t, rawURL, "kept")
	ctx := t.Context()

	record := func(st *Store, n int) {
		t.Helper()

		hash := fmt.Sprintf("%064x", n)

		err := st.RecordNode(ctx, store.NodeRecord{
			Hash: hash, Kind: "task", Resource: "step", Content: map[string]any{"hash": hash},
		}, "build", "succeeded", nil, nil)
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}
	}

	orphans := func(when string) {
		t.Helper()

		got := queryInt(t, st.db, `SELECT COUNT(*) FROM node_content c WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.content_hash = c.content_hash)`)
		if got != 0 {
			t.Errorf("%s: %d node_content rows no node points at", when, got)
		}
	}

	for n := range store.NodesPerRetainedRun + 1 {
		record(st, n+1)
	}

	err := st.Prune(ctx, store.Retention{JobName: "build", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	orphans("after the node cap reaped one")

	deleted := openStore(t, rawURL, "deleted")
	record(deleted, 1_000)

	err = deleted.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	orphans("after a pipeline was deleted")
}
