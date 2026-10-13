package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// A daemon holds one handle per pipeline on ONE file. Each used to open its
// own writer, so N pipelines were N writers taking turns through sqlite's
// sleep-and-retry busy handler — measured at 100 pipelines: polls, claims and
// run events failing "database is locked" once a wait passed busy_timeout.
// Handles on one file share one writer, so a write queues on the pool instead.
func TestHandlesOnOneFileShareTheirPools(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	first := mustOpenStoreNamed(t, path, "first")
	second := mustOpenStoreNamed(t, path, "second")

	if first.db != second.db || first.reads != second.reads {
		t.Fatal("two handles on one file opened pools of their own")
	}

	other := mustOpenStoreNamed(t, filepath.Join(t.TempDir(), "other.db"), "first")
	if other.db == first.db {
		t.Fatal("handles on different files share a pool")
	}

	err := first.Release()
	if err != nil {
		t.Fatalf("release first: %v", err)
	}

	// The neighbour's release must not have closed what this handle writes through.
	err = second.StartRun(context.Background(), "run-1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("second handle after the first released: %v", err)
	}

	err = second.Close()
	if err != nil {
		t.Fatalf("close second: %v", err)
	}

	err = other.Close()
	if err != nil {
		t.Fatalf("close other: %v", err)
	}

	// Last one out closed the pool, and reopening starts a fresh one.
	again := mustOpenStoreNamed(t, path, "first")
	defer func() { _ = again.Close() }()

	if again.db == first.db {
		t.Fatal("a pool every handle released was handed out again")
	}
}

func TestManyHandlesWriteConcurrently(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	const handles, writes = 20, 25

	var wg sync.WaitGroup

	errs := make(chan error, handles*writes)

	for h := range handles {
		st := mustOpenStoreNamed(t, path, fmt.Sprintf("p%d", h))
		defer func() { _ = st.Close() }()

		runID := fmt.Sprintf("run-%d", h)

		wg.Go(func() {
			ctx := context.Background()

			err := st.StartRun(ctx, runID, "build", "/tmp/ws", "")
			if err != nil {
				errs <- err

				return
			}

			for i := range writes {
				errs <- st.AppendRunEvent(ctx, store.RunEventRow{RunID: runID, Type: "step_output_chunk", Text: strconv.Itoa(i), At: time.Now()})
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func mustOpenStoreNamed(t *testing.T, path, name string) *Store {
	t.Helper()

	st, err := OpenStore(path, name)
	if err != nil {
		t.Fatalf("open %s in %s: %v", name, path, err)
	}

	return st
}

// Every pipeline's drain worker asks four times a second whether anything is
// queued. When nothing is, the answer must not need the file's one writer: at
// 100 pipelines those idle claims were 400 write transactions a second,
// queued in front of every build's real writes.
func TestAnIdleClaimDoesNotWaitForTheWriter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	busy := mustOpenStoreNamed(t, path, "busy")
	defer func() { _ = busy.Close() }()

	idle := mustOpenStoreNamed(t, path, "idle")
	defer func() { _ = idle.Close() }()

	ctx := context.Background()

	tx, err := busy.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, _, claimed, err := idle.ClaimNextJob(bounded)
	if err != nil {
		t.Fatalf("an empty queue's claim waited on the writer: %v", err)
	}

	if claimed {
		t.Fatal("claimed from an empty queue")
	}
}

// database/sql keeps two idle connections unless told otherwise, so a pool
// wider than that closed every connection a burst of reads opened past the
// second and opened it again for the next burst — pragmas, page cache and
// all. A daemon's drain workers are exactly such a burst, every quarter second.
func TestReadPoolKeepsTheConnectionsItOpens(t *testing.T) {
	t.Parallel()

	st := mustOpenStoreNamed(t, filepath.Join(t.TempDir(), "state.db"), "p")
	defer func() { _ = st.Close() }()

	for range 5 {
		var wg sync.WaitGroup

		for range readPoolSize * 2 {
			wg.Go(func() {
				_, err := st.Paused(context.Background())
				if err != nil {
					t.Error(err)
				}
			})
		}

		wg.Wait()
	}

	if closed := st.reads.Stats().MaxIdleClosed; closed > 0 {
		t.Fatalf("the read pool closed %d connections for want of idle room", closed)
	}
}
