package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

func recordNodeWith(t *testing.T, st *Store, hash string, content map[string]any) {
	t.Helper()

	err := st.RecordNode(t.Context(), store.NodeRecord{Hash: hash, Kind: "task", Resource: "step", Content: content}, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode %s: %v", hash, err)
	}
}

// TestPruneThatReapsNoNodeNeverTakesTheContentLock: the EXCLUSIVE content lock stalls every pipeline's RecordNode, so a build that freed nothing must not ask for it.
func TestPruneThatReapsNoNodeNeverTakesTheContentLock(t *testing.T) {
	t.Parallel()

	st := openStore(t, newDatabase(t), "quiet")

	recordNodeWith(t, st, fmt.Sprintf("%064x", 1), map[string]any{"a": 1})

	holder, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = holder.Rollback() }()

	_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1, 0)`, classContent)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err = st.Prune(ctx, store.Retention{JobName: "build", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune with the content lock held elsewhere and nothing to reap: %v", err)
	}
}
