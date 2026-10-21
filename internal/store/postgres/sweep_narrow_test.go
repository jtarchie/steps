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

func contentRows(t *testing.T, st *Store, where string) int {
	t.Helper()

	return queryInt(t, st.db, `SELECT COUNT(*) FROM node_content WHERE `+where)
}

// TestPruneSweepsOnlyTheContentItFreed: the sweep after a node prune looks at the deleted nodes' content and nothing else, so a row another pipeline's node still names survives, a freed one goes, and an orphan from elsewhere is left for Delete.
func TestPruneSweepsOnlyTheContentItFreed(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	mine := openStore(t, rawURL, "mine")
	other := openStore(t, rawURL, "other")

	recordNodeWith(t, mine, fmt.Sprintf("%064x", 1), map[string]any{"shared": true})
	recordNodeWith(t, other, fmt.Sprintf("%064x", 1), map[string]any{"shared": true})
	recordNodeWith(t, mine, fmt.Sprintf("%064x", 2), map[string]any{"freed": true})

	for n := range store.NodesPerRetainedRun - 2 {
		recordNodeWith(t, mine, fmt.Sprintf("%064x", n+3), map[string]any{"own": n})
	}

	recordNodeWith(t, mine, fmt.Sprintf("%064x", 1_000), map[string]any{"newest": true})
	recordNodeWith(t, mine, fmt.Sprintf("%064x", 1_001), map[string]any{"newest": 2})

	_, err := mine.db.ExecContext(t.Context(), `INSERT INTO node_content (content_hash, content) VALUES ('stray', '{}')`)
	if err != nil {
		t.Fatal(err)
	}

	err = mine.Prune(t.Context(), store.Retention{JobName: "build", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got := queryInt(t, mine.db, fmt.Sprintf(`SELECT COUNT(*) FROM nodes WHERE pipeline_id = %d`, mine.pipelineID)); got != store.NodesPerRetainedRun {
		t.Fatalf("%d nodes survived, want %d: the prune did not reap the two oldest", got, store.NodesPerRetainedRun)
	}

	shared := `content = '{"shared":true}'`
	if got := contentRows(t, mine, shared); got != 1 {
		t.Errorf("%d shared content rows, want 1: a sweep that ignores the other pipeline's reference deleted it", got)
	}

	freed := `content = '{"freed":true}'`
	if got := contentRows(t, mine, freed); got != 0 {
		t.Errorf("%d freed content rows, want 0: the candidate nobody references was not swept", got)
	}

	if got := contentRows(t, mine, `content_hash = 'stray'`); got != 1 {
		t.Errorf("Prune swept a stray row it never freed; only Delete sweeps the whole table")
	}

	err = other.Delete(t.Context())
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := contentRows(t, mine, `content_hash = 'stray'`); got != 0 {
		t.Errorf("Delete left the stray row: its sweep must stay table-wide")
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
