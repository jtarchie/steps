package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// journalMode reports the sqlite journal mode of the store's database.
func journalMode(t *testing.T, store *Store) string {
	t.Helper()

	var mode string

	err := store.db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}

	return mode
}

func TestStoreUsesWAL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	mode := journalMode(t, store)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

// TestStoreConcurrentOpenEnablesWAL opens the same brand-new database from
// many goroutines at once — the concurrent first-access case that can
// transiently return SQLITE_BUSY during WAL conversion — and asserts every
// open succeeds and the database ends up in WAL mode.
func TestStoreConcurrentOpenEnablesWAL(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	const openers = 8

	var wg sync.WaitGroup

	errs := make(chan error, openers)

	for range openers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			store, err := OpenStore(path)
			if err != nil {
				errs <- err

				return
			}

			_ = store.Close()
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent OpenStore: %v", err)
	}

	store := mustOpenStore(t, path)
	defer func() { _ = store.Close() }()

	mode := journalMode(t, store)
	if mode != "wal" {
		t.Errorf("journal_mode after concurrent open = %q, want %q", mode, "wal")
	}
}

func mustOpenStore(t *testing.T, path string) *Store {
	t.Helper()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	return store
}

func assertHasSucceeded(t *testing.T, store *Store, jobName, rootHash string, want bool) {
	t.Helper()

	got, err := store.HasSucceeded(context.Background(), jobName, rootHash)
	if err != nil {
		t.Fatalf("HasSucceeded(%q, %q): %v", jobName, rootHash, err)
	}

	if got != want {
		t.Errorf("HasSucceeded(%q, %q) = %v, want %v", jobName, rootHash, got, want)
	}
}

func mustRecordJobRun(t *testing.T, store *Store, jobName, rootHash, status string, runErr error) {
	t.Helper()

	err := store.RecordJobRun(context.Background(), jobName, rootHash, status, runErr)
	if err != nil {
		t.Fatalf("RecordJobRun(%q, %q, %q): %v", jobName, rootHash, status, err)
	}
}

func TestStoreHasSucceededAndRecordJobRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.db")

	store := mustOpenStore(t, path)

	_, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("expected parent directory to be created: %v", err)
	}

	assertHasSucceeded(t, store, "job", "hash1", false)

	mustRecordJobRun(t, store, "job", "hash1", "succeeded", nil)
	assertHasSucceeded(t, store, "job", "hash1", true)
	assertHasSucceeded(t, store, "job", "hash2", false)
	assertHasSucceeded(t, store, "other-job", "hash1", false)

	mustRecordJobRun(t, store, "job", "hash3", "failed", errors.New("boom"))
	assertHasSucceeded(t, store, "job", "hash3", false)

	err = store.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := mustOpenStore(t, path)
	defer func() { _ = reopened.Close() }()

	assertHasSucceeded(t, reopened, "job", "hash1", true)
}

func TestStoreRecordNode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := mustOpenStore(t, filepath.Join(dir, "state.db"))

	defer func() { _ = store.Close() }()

	ctx := context.Background()

	node := Node{
		Hash:       "abc",
		ParentHash: "",
		Kind:       NodeKindGet,
		StepIndex:  0,
		Resource:   "thing",
		Content:    map[string]any{"source": map[string]any{"key": "v1"}},
	}

	err := store.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	// Recording the same hash again (an upsert) should not error.
	err = store.RecordNode(ctx, node, "job", "succeeded", map[string]any{"ref": "v1"}, nil)
	if err != nil {
		t.Fatalf("RecordNode (upsert): %v", err)
	}
}
