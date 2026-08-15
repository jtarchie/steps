package store

import (
	"path/filepath"
	"testing"
)

// TestWatchLockIsExclusiveAndReleasable pins the property the queue reset
// depends on: at most one live watcher per state.db, detected by a lock that
// dies with its process rather than by a record that would outlive one.
func TestWatchLockIsExclusiveAndReleasable(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	first := mustOpenStore(t, path)
	defer func() { _ = first.Close() }()

	release, held, err := first.AcquireWatchLock()
	if err != nil {
		t.Fatalf("AcquireWatchLock: %v", err)
	}

	if held {
		t.Fatal("a fresh lock reported as held")
	}

	// A second process (a second handle stands in for one; flock is
	// per-file-description, not per-process-wide fd table).
	second := mustOpenStore(t, path)
	defer func() { _ = second.Close() }()

	_, heldNow, err := second.AcquireWatchLock()
	if err != nil {
		t.Fatalf("AcquireWatchLock (second): %v", err)
	}

	if !heldNow {
		t.Fatal("two watchers acquired the lock at once — ResetStaleRunning would claim each other's builds")
	}

	release()

	releaseAgain, heldAfter, err := second.AcquireWatchLock()
	if err != nil {
		t.Fatalf("AcquireWatchLock (after release): %v", err)
	}

	if heldAfter {
		t.Fatal("the lock stayed held after release")
	}

	releaseAgain()
}
