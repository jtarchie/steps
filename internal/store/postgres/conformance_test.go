package postgres

import (
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/storetest"
)

// TestStoreConformance runs the store's conformance suite against a real
// Postgres: the same tests the sqlite driver passes.
func TestStoreConformance(t *testing.T) {
	t.Parallel()
	requirePostgres(t)

	// One database per test, shared by every pipeline name that test opens:
	// the multi-pipeline cases are about two handles on ONE database.
	var (
		mu        sync.Mutex
		databases = map[string]string{}
	)

	storetest.Run(t, func(t *testing.T, pipeline string) store.Store {
		t.Helper()

		mu.Lock()

		rawURL, ok := databases[t.Name()]
		if !ok {
			rawURL = newDatabase(t)
			databases[t.Name()] = rawURL
		}

		mu.Unlock()

		st, err := OpenStore(rawURL, pipeline)
		if err != nil {
			t.Fatalf("OpenStore(%q): %v", pipeline, err)
		}

		return st
	})
}
