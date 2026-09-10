package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// journalMode reports the sqlite journal mode of the st's database.
func journalMode(t *testing.T, st *Store) string {
	t.Helper()

	var mode string

	err := st.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}

	return mode
}

func TestStoreUsesWAL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = st.Close() }()

	mode := journalMode(t, st)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func mustOpenStore(t *testing.T, path string) *Store {
	t.Helper()

	st, err := OpenStore(path, "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	return st
}

// Measured: a destroy that freed a gigabyte held the file's write lock 31s reclaiming it, and a neighbour's queue writes failed SQLITE_BUSY until it let go.
func TestReleaseLeavesTheReclaimToClose(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "steps.db")

	doomed, err := OpenStore(path, "doomed")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	neighbour, err := OpenStore(path, "neighbour")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	for i := range 16 {
		err = doomed.RecordRevision(ctx, fmt.Sprintf("sha-%d", i), strconv.Itoa(i)+strings.Repeat("x", 64<<10), nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}
	}

	err = doomed.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	err = doomed.Release()
	if err != nil {
		t.Fatalf("Release: %v", err)
	}

	if freePages(t, path) == 0 {
		t.Error("Release reclaimed what the delete freed, under the write lock a neighbour on the file is still writing through")
	}

	err = neighbour.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if free := freePages(t, path); free != 0 {
		t.Errorf("Close left %d freed pages unreclaimed", free)
	}
}

// freePages is what deletes freed and no reclaim has handed back yet.
func freePages(t *testing.T, path string) int {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	defer func() { _ = db.Close() }()

	var free int

	err = db.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&free)
	if err != nil {
		t.Fatalf("freelist_count: %v", err)
	}

	return free
}

func assertHasSucceeded(t *testing.T, st *Store, jobName, rootHash string, want bool) {
	t.Helper()

	found, err := st.HasSucceededBatch(context.Background(), jobName, []string{rootHash})
	if err != nil {
		t.Fatalf("HasSucceededBatch(%q, %q): %v", jobName, rootHash, err)
	}

	if got := found[rootHash]; got != want {
		t.Errorf("HasSucceededBatch(%q, %q) = %v, want %v", jobName, rootHash, got, want)
	}
}

// mustRecordNode records a minimal node under a hash, so a row that references
// it has something to point at.
//
// agent_usage.node_hash and node_transcripts.hash are foreign keys into nodes,
// which is what lets retention delete a node and take its dependents with it.
// job_runs.root_hash deliberately is NOT one (see the schema, and the negative
// assertion in footprint_test.go) — but a job_runs row still names a chain leaf,
// and seeding the node it names keeps these fixtures shaped like the runs that
// produce them.
//
// The node is recorded as SUCCEEDED, which is a cache-hit state: a test about
// HasNodeSucceeded or across-cell memoization should record its own nodes rather
// than inherit this one.
func mustRecordNode(t *testing.T, st *Store, jobName, hash string) {
	t.Helper()

	err := st.RecordNode(context.Background(), store.NodeRecord{
		Hash: hash, Kind: "task", Resource: "step", Content: map[string]any{"hash": hash},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode(%q, %q): %v", jobName, hash, err)
	}
}

func mustRecordChainSucceeded(t *testing.T, st *Store, jobName, rootHash string) {
	t.Helper()

	mustRecordNode(t, st, jobName, rootHash)

	err := st.RecordChainSucceeded(context.Background(), jobName, rootHash)
	if err != nil {
		t.Fatalf("RecordChainSucceeded(%q, %q): %v", jobName, rootHash, err)
	}
}

func TestStoreHasSucceededAndRecordChain(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.db")

	st := mustOpenStore(t, path)

	_, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("expected parent directory to be created: %v", err)
	}

	assertHasSucceeded(t, st, "job", "hash1", false)

	mustRecordChainSucceeded(t, st, "job", "hash1")
	assertHasSucceeded(t, st, "job", "hash1", true)
	assertHasSucceeded(t, st, "job", "hash2", false)
	assertHasSucceeded(t, st, "other-job", "hash1", false)

	err = st.ForgetChain(context.Background(), "job", "hash3")
	if err != nil {
		t.Fatalf("ForgetChain: %v", err)
	}

	assertHasSucceeded(t, st, "job", "hash3", false)

	err = st.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := mustOpenStore(t, path)
	defer func() { _ = reopened.Close() }()

	assertHasSucceeded(t, reopened, "job", "hash1", true)
}

func mustEnqueueJob(t *testing.T, st *Store, jobName, reason string) {
	t.Helper()

	err := st.EnqueueJob(context.Background(), jobName, reason)
	if err != nil {
		t.Fatalf("EnqueueJob(%q, %q): %v", jobName, reason, err)
	}
}

// mustClaimJob claims the next pending job and fails the test if the queue
// was empty or the claimed job doesn't match want (when want != "").
func mustClaimJob(t *testing.T, st *Store, want string) (id int64, jobName string) {
	t.Helper()

	id, jobName, found, err := st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if !found {
		t.Fatal("ClaimNextJob: expected a pending job, queue was empty")
	}

	if want != "" && jobName != want {
		t.Fatalf("ClaimNextJob = %q, want %q", jobName, want)
	}

	return id, jobName
}

func TestStoreCompleteJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = st.Close() }()

	ctx := context.Background()

	mustEnqueueJob(t, st, "build", "resource")

	id, _ := mustClaimJob(t, st, "build")

	err := st.CompleteJob(ctx, id, "failed", errors.New("boom"))
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	var (
		status  string
		errText *string
	)

	scanErr := st.db.QueryRowContext(ctx, "SELECT status, error FROM trigger_queue WHERE id = ?", id).Scan(&status, &errText)
	if scanErr != nil {
		t.Fatalf("scan trigger_queue row: %v", scanErr)
	}

	if status != "failed" {
		t.Errorf("status = %q, want %q", status, "failed")
	}

	if errText == nil || *errText != "boom" {
		t.Errorf("error = %v, want %q", errText, "boom")
	}
}
