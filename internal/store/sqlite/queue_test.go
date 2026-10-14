package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestResetStaleRunningIsAllOrNothing: four statements re-queue a dead
// process's jobs and close its runs. Run as autocommits, a failure part way
// re-queued the jobs while their runs still read as running — and the next
// startup, finding nothing running in the queue, never closed them.
func TestResetStaleRunningIsAllOrNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	t.Cleanup(func() { _ = st.Close() })

	mustStartRun(t, st, "r1", "build")

	err := st.EnqueueJob(ctx, "build", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	_, found, err := st.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("ClaimNextJob = %v, %v", found, err)
	}

	// The last of the four statements fails.
	_, err = st.db.ExecContext(ctx, `
		CREATE TRIGGER refuse_abort BEFORE UPDATE OF status ON runs
		BEGIN SELECT RAISE(ABORT, 'refused'); END
	`)
	if err != nil {
		t.Fatal(err)
	}

	err = st.ResetStaleRunning(ctx)
	if err == nil {
		t.Fatal("ResetStaleRunning succeeded with closing the runs refused")
	}

	var status string

	err = st.db.QueryRowContext(ctx, `SELECT status FROM trigger_queue WHERE pipeline_id = ?`, st.pipelineID).Scan(&status)
	if err != nil {
		t.Fatal(err)
	}

	if status != "running" {
		t.Errorf("queue row is %q after a failed reset, want it left running", status)
	}
}
