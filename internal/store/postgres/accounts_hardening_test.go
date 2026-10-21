package postgres

import (
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func TestFindUserRefusesACorruptGroupsColumn(t *testing.T) {
	accounts, err := OpenAccounts(newDatabase(t))
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
	rawURL := newDatabase(t)

	accounts, err := OpenAccounts(rawURL)
	if err != nil {
		t.Fatal(err)
	}

	st := openStore(t, rawURL, "p")

	for range 2 {
		err = accounts.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	err = st.StartRun(t.Context(), store.NewRun{ID: "run-1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("a Store sharing the URL after a double Accounts.Close: %v", err)
	}
}
