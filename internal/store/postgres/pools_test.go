package postgres

import (
	"context"
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
