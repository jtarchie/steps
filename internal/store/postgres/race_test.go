package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestRecordNodeSurvivesAnotherPipelinesContentSweep: node_content is the one
// table pipelines share. Pipeline A recording a node over content pipeline B
// already interned finds the row present, writes nothing, and then points its
// node at it. B deleting itself sweeps the content its nodes held — and if
// that sweep lands between A's two statements, A's reference names a row that
// is gone. sqlite serialized the file, so the window never existed there.
//
// The window is one statement wide, so the test holds it open rather than
// hoping to hit it: an uncommitted row with A's node key parks A's second
// statement on the unique index, B deletes in the meantime, and only then is
// A let go.
func TestRecordNodeSurvivesAnotherPipelinesContentSweep(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	ctx := context.Background()
	pipelineA := openStore(t, rawURL, "a")
	pipelineB := openStore(t, rawURL, "b")
	shared := map[string]any{"body": "shared"}

	err := pipelineB.RecordNode(ctx, store.NodeRecord{Hash: "b", Kind: "task", Content: shared}, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("B's RecordNode: %v", err)
	}

	db := rawDB(t, rawURL)
	blocker := holdNodeKey(t, db, pipelineA.pipelineID, "a")

	recorded := make(chan error, 1)

	go func() {
		recorded <- pipelineA.RecordNode(ctx, store.NodeRecord{Hash: "a", Kind: "task", Content: shared}, "build", "succeeded", nil, nil)
	}()

	waitForLockWaiters(t, db, 1)

	deleted := make(chan error, 1)

	go func() { deleted <- pipelineB.Delete(ctx) }()

	// Either B finished — the sweep ran inside A's window — or it is parked
	// behind A, which is the lock doing its job. Both are the moment to let A go.
	select {
	case err = <-deleted:
		deleted <- err
	case <-lockWaiters(t, db, 2):
	}

	err = blocker.Rollback()
	if err != nil {
		t.Fatal(err)
	}

	err = <-recorded
	if err != nil {
		t.Errorf("A's RecordNode: %v — B's sweep took the content A's node points at", err)
	}

	err = <-deleted
	if err != nil {
		t.Errorf("B's Delete: %v", err)
	}

	found, err := pipelineA.NodesByHash(ctx, []string{"a"})
	if err != nil || len(found) != 1 {
		t.Errorf("NodesByHash = %d rows (%v), want A's node and its content", len(found), err)
	}
}

// holdNodeKey inserts, and leaves uncommitted, a node under the key a
// RecordNode is about to write, so that write parks on the unique index until
// the returned transaction ends. Its own content, so it holds no lock on any
// row another pipeline's sweep must delete.
func holdNodeKey(t *testing.T, db *sql.DB, pipelineID int64, hash string) *sql.Tx {
	t.Helper()

	ctx := context.Background()

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = blocker.Rollback() })

	_, err = blocker.ExecContext(ctx, `INSERT INTO steps.node_content VALUES ('blocker', '{}')`)
	if err == nil {
		_, err = blocker.ExecContext(ctx, `
			INSERT INTO steps.nodes (pipeline_id, hash, kind, job_name, resource, step_index, content_hash, status, created_at)
			VALUES ($1, $2, 'task', 'build', '', 0, 'blocker', 'succeeded', now())
		`, pipelineID, hash)
	}

	if err != nil {
		t.Fatal(err)
	}

	return blocker
}

// waitForLockWaiters blocks until n sessions in this database wait on a lock.
func waitForLockWaiters(t *testing.T, db *sql.DB, n int) {
	t.Helper()

	select {
	case <-lockWaiters(t, db, n):
	case <-time.After(10 * time.Second):
		t.Fatalf("never saw %d sessions waiting on a lock", n)
	}
}

func lockWaiters(t *testing.T, db *sql.DB, n int) <-chan struct{} {
	t.Helper()

	reached := make(chan struct{})

	go func() {
		defer close(reached)

		for range 1000 {
			var waiting int

			err := db.QueryRowContext(context.Background(), `
				SELECT COUNT(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
			`).Scan(&waiting)
			if err != nil || waiting >= n {
				return
			}

			time.Sleep(10 * time.Millisecond)
		}
	}()

	return reached
}

// TestTheLockHolderCannotIdleForever: a client that vanishes mid-write
// leaves its backend holding the pipeline lock until the server notices the
// dead peer — hours, by TCP keepalive — so the transaction bounds its own
// idleness.
func TestTheLockHolderCannotIdleForever(t *testing.T) {
	t.Parallel()

	st := openStore(t, newDatabase(t), "p")

	var limit string

	err := st.write(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&limit)
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if limit == "0" || limit == "" {
		t.Errorf("idle_in_transaction_session_timeout is %q inside write, want a bound", limit)
	}
}
