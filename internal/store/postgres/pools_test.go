package postgres

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// A daemon holds a handle per pipeline, and each used to open four
// connections of its own: 25 pipelines met Postgres's default
// max_connections. Handles on one URL share one pool.
func TestHandlesOnOneURLShareAPool(t *testing.T) {
	rawURL := newDatabase(t)

	first, err := OpenStore(rawURL, "first")
	if err != nil {
		t.Fatal(err)
	}

	second := openStore(t, rawURL, "second")

	if first.db != second.db {
		t.Fatal("two handles on one URL opened pools of their own")
	}

	err = first.Release()
	if err != nil {
		t.Fatal(err)
	}

	err = first.Release()
	if err != nil {
		t.Fatalf("a second release: %v", err)
	}

	// The neighbour's release must not have closed what this handle writes through.
	err = second.StartRun(context.Background(), store.NewRun{ID: "run-1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("second handle after the first released: %v", err)
	}
}

// database/sql keeps two idle connections by default, so a pool wider than
// that closed what a burst opened and redialled it — TCP, TLS and auth — for
// the next. A daemon's drain workers are such a burst every quarter second.
func TestPoolsKeepTheConnectionsTheyOpen(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	shared := openStore(t, rawURL, "p")

	existing, err := OpenExisting(rawURL, "p")
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}

	t.Cleanup(func() { _ = existing.Close() })

	for name, db := range map[string]*sql.DB{"shared": shared.db, "existing": existing.db} {
		width := db.Stats().MaxOpenConnections

		for range 3 {
			var wg sync.WaitGroup

			for range width {
				wg.Go(func() {
					_, err := db.ExecContext(t.Context(), `SELECT pg_sleep(0.05)`)
					if err != nil {
						t.Error(err)
					}
				})
			}

			wg.Wait()
		}

		if closed := db.Stats().MaxIdleClosed; closed > 0 {
			t.Errorf("the %s pool closed %d connections for want of idle room", name, closed)
		}
	}
}
