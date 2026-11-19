package sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/storetest"
)

// TestStoreConformance runs the store's conformance suite against this driver.
//
// Everything it asserts is written against internal/store's contract, so a
// second driver proves itself by adding one test exactly like this one. What
// stays in this package is what measures SQLite itself — the file footprint,
// the pragmas, the schema stamp — and nothing a caller of store.Store can see.
func TestStoreConformance(t *testing.T) {
	t.Parallel()

	// One database per test, shared by every pipeline name that test opens:
	// the multi-pipeline cases are about two handles on ONE file, and a
	// t.TempDir() per call would hand them two.
	root := t.TempDir()

	storetest.Run(t, func(t *testing.T, pipeline string) store.Store {
		t.Helper()

		path := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "-"), "state.db")

		st, err := OpenStore(path, pipeline)
		if err != nil {
			t.Fatalf("OpenStore(%q, %q): %v", path, pipeline, err)
		}

		return st
	})
}

// TestAccountsConformance runs the accounts conformance suite against this driver.
func TestAccountsConformance(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	storetest.RunAccounts(t, func(t *testing.T) (store.Accounts, storetest.OpenFunc) {
		t.Helper()

		path := filepath.Join(root, strings.ReplaceAll(t.Name(), "/", "-"), "state.db")

		accounts, err := OpenAccounts(path)
		if err != nil {
			t.Fatalf("OpenAccounts(%q): %v", path, err)
		}

		return accounts, func(t *testing.T, pipeline string) store.Store {
			t.Helper()

			st, err := OpenStore(path, pipeline)
			if err != nil {
				t.Fatalf("OpenStore(%q, %q): %v", path, pipeline, err)
			}

			return st
		}
	}, func(t *testing.T, accounts store.Accounts, clientID string) {
		t.Helper()

		sqliteAccounts, ok := accounts.(*Accounts)
		if !ok {
			t.Fatalf("accounts is %T, want *Accounts", accounts)
		}

		_, err := sqliteAccounts.held.db.ExecContext(t.Context(), `UPDATE oauth_clients SET redirect_uris = 'not json' WHERE id = ?`, clientID)
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestContentSweepConformance runs the narrowed node_content sweep's conformance test against this driver.
func TestContentSweepConformance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	raw := func(t *testing.T, st store.Store) *Store {
		t.Helper()

		sqliteStore, ok := st.(*Store)
		if !ok {
			t.Fatalf("store is %T, want *Store", st)
		}

		return sqliteStore
	}

	storetest.RunContentSweep(t, func(t *testing.T, pipeline string) store.Store {
		t.Helper()

		st, err := OpenStore(path, pipeline)
		if err != nil {
			t.Fatalf("OpenStore(%q, %q): %v", path, pipeline, err)
		}

		return st
	}, storetest.ContentProbe{
		Plant: func(t *testing.T, st store.Store, contentHash string) {
			t.Helper()

			_, err := raw(t, st).db.ExecContext(t.Context(), `INSERT INTO node_content (content_hash, content) VALUES (?, '{}')`, contentHash)
			if err != nil {
				t.Fatal(err)
			}
		},
		Rows: func(t *testing.T, st store.Store, contentHash string) int {
			t.Helper()

			var count int

			err := raw(t, st).db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM node_content WHERE content_hash = ?`, contentHash).Scan(&count)
			if err != nil {
				t.Fatal(err)
			}

			return count
		},
	})
}
