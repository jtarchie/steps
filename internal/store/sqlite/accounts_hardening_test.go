package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func TestFindUserRefusesACorruptGroupsColumn(t *testing.T) {
	t.Parallel()

	accounts, err := OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	err = accounts.SaveUser(t.Context(), store.User{Actor: "u", Groups: []string{"admins"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = accounts.held.db.ExecContext(t.Context(), `UPDATE users SET groups_json = 'not json' WHERE actor = 'u'`)
	if err != nil {
		t.Fatal(err)
	}

	_, found, err := accounts.FindUser(t.Context(), "u")
	if err == nil || found {
		t.Fatalf("FindUser over a corrupt groups column = found %v, err %v; want an error", found, err)
	}
}

func TestAccountsCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	accounts, err := OpenAccounts(path)
	if err != nil {
		t.Fatal(err)
	}

	st := mustOpenStoreNamed(t, path, "p")
	t.Cleanup(func() { _ = st.Close() })

	for range 2 {
		err = accounts.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	err = st.StartRun(t.Context(), store.NewRun{ID: "run-1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("a Store sharing the file after a double Accounts.Close: %v", err)
	}
}
