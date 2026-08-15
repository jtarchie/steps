package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentEnqueueAcrossProcesses pins the transaction mode.
//
// Enqueuing became a read-modify-write when queue rows started carrying
// versions to merge, which is exactly the shape SQLite refuses to let wait:
// a deferred transaction that reads and then writes must UPGRADE its lock,
// and SQLite returns SQLITE_BUSY immediately rather than honoring
// busy_timeout, since waiting there could deadlock. _txlock=immediate takes
// the write lock up front instead (see OpenStore).
//
// Two handles on one file is `steps watch` alongside `steps web`, both of
// which enqueue. Within a single process SetMaxOpenConns(1) makes this
// unreachable, so a same-handle test would pass while the real shape failed.
func TestConcurrentEnqueueAcrossProcesses(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	a := mustOpenStore(t, path)
	defer func() { _ = a.Close() }()
	b := mustOpenStore(t, path)
	defer func() { _ = b.Close() }()

	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 32)

	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := a
			if i%2 == 1 {
				st = b
			}
			err := st.EnqueueJobWithVersions(ctx, "build", "items", QueuedVersions{"items": {{"n": "1"}}})
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}

	if len(failures) > 0 {
		t.Errorf("%d/16 concurrent enqueues failed, want none: %v", len(failures), failures[0])
	}
}
