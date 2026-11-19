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

// TestAccountsConformance runs the accounts conformance suite against a real Postgres.
func TestAccountsConformance(t *testing.T) {
	t.Parallel()
	requirePostgres(t)

	storetest.RunAccounts(t, func(t *testing.T) (store.Accounts, storetest.OpenFunc) {
		t.Helper()

		rawURL := newDatabase(t)

		accounts, err := OpenAccounts(rawURL)
		if err != nil {
			t.Fatalf("OpenAccounts: %v", err)
		}

		return accounts, func(t *testing.T, pipeline string) store.Store {
			t.Helper()

			st, err := OpenStore(rawURL, pipeline)
			if err != nil {
				t.Fatalf("OpenStore(%q): %v", pipeline, err)
			}

			return st
		}
	}, func(t *testing.T, accounts store.Accounts, clientID string) {
		t.Helper()

		pgAccounts, ok := accounts.(*Accounts)
		if !ok {
			t.Fatalf("accounts is %T, want *Accounts", accounts)
		}

		_, err := pgAccounts.held.db.ExecContext(t.Context(), `UPDATE oauth_clients SET redirect_uris = 'not json' WHERE id = $1`, clientID)
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestContentSweepConformance runs the narrowed node_content sweep's conformance test against a real Postgres.
func TestContentSweepConformance(t *testing.T) {
	t.Parallel()
	requirePostgres(t)

	rawURL := newDatabase(t)

	raw := func(t *testing.T, st store.Store) *Store {
		t.Helper()

		pgStore, ok := st.(*Store)
		if !ok {
			t.Fatalf("store is %T, want *Store", st)
		}

		return pgStore
	}

	storetest.RunContentSweep(t, func(t *testing.T, pipeline string) store.Store {
		t.Helper()

		st, err := OpenStore(rawURL, pipeline)
		if err != nil {
			t.Fatalf("OpenStore(%q): %v", pipeline, err)
		}

		return st
	}, storetest.ContentProbe{
		Plant: func(t *testing.T, st store.Store, contentHash string) {
			t.Helper()

			_, err := raw(t, st).db.ExecContext(t.Context(), `INSERT INTO node_content (content_hash, content) VALUES ($1, '{}')`, contentHash)
			if err != nil {
				t.Fatal(err)
			}
		},
		Rows: func(t *testing.T, st store.Store, contentHash string) int {
			t.Helper()

			var count int

			err := raw(t, st).db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM node_content WHERE content_hash = $1`, contentHash).Scan(&count)
			if err != nil {
				t.Fatal(err)
			}

			return count
		},
	})
}
