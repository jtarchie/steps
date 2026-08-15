package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentEnqueueAcrossProcesses keeps two processes able to write at
// once.
//
// Two handles on one file is `steps watch` alongside `steps web`, both of
// which enqueue; within a single process SetMaxOpenConns(1) makes contention
// unreachable, so a same-handle test would pass while the real shape failed.
//
// Enqueuing is a single statement again now that a row carries no versions
// to merge, but the store still runs read-modify-write transactions
// elsewhere (RecordVersions assigns check_order from a MAX it just read), and
// those are the shape SQLite refuses to let wait: a deferred transaction that
// reads and then writes must UPGRADE its lock, and SQLite answers SQLITE_BUSY
// immediately rather than honoring busy_timeout, since waiting there could
// deadlock. _txlock=immediate takes the write lock up front — see OpenStore.
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
			err := st.EnqueueJob(ctx, "build", "items")
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
