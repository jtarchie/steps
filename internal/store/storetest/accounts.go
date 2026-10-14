package storetest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// AccountsOpenFunc opens the daemon-level accounts handle on a fresh database.
type AccountsOpenFunc func(t *testing.T) store.Accounts

// accountsSuite is the conformance suite for store.Accounts, found by reflection for the reason suite is.
type accountsSuite struct {
	open AccountsOpenFunc
}

// RunAccounts runs the accounts conformance suite against one driver.
func RunAccounts(t *testing.T, open AccountsOpenFunc) {
	t.Helper()

	closing := func(t *testing.T) store.Accounts {
		t.Helper()

		accounts := open(t)
		t.Cleanup(func() { _ = accounts.Close() })

		return accounts
	}

	value := reflect.ValueOf(&accountsSuite{open: closing})
	typ := value.Type()

	for index := range typ.NumMethod() {
		method := typ.Method(index)
		if !strings.HasPrefix(method.Name, "Test") {
			t.Fatalf("%s is exported but is not a Test, so nobody would run it", method.Name)
		}

		test, ok := value.Method(index).Interface().(func(*testing.T))
		if !ok {
			t.Fatalf("%s is %s, want func(*testing.T)", method.Name, method.Type)
		}

		t.Run(method.Name, test)
	}
}

// TestASessionIsFoundUntilRevoked: signing out ends a session at once, for every request after it.
func (s accountsSuite) TestASessionIsFoundUntilRevoked(t *testing.T) {
	t.Parallel()

	accounts := s.open(t)
	created := time.Now().UTC().Truncate(time.Second)

	err := accounts.CreateSession(ctxFor(t), store.Session{TokenHash: "digest", Actor: "local:alex", CreatedAt: created, ExpiresAt: created.Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	found, ok, err := accounts.FindSession(ctxFor(t), "digest")
	if err != nil || !ok || found.Actor != "local:alex" || !found.ExpiresAt.Equal(created.Add(time.Hour)) {
		t.Fatalf("FindSession = %+v, %v, %v; want alex's session", found, ok, err)
	}

	err = accounts.RevokeSession(ctxFor(t), "digest")
	if err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	_, ok, err = accounts.FindSession(ctxFor(t), "digest")
	if err != nil || ok {
		t.Fatalf("FindSession after revoke = %v, %v; want gone", ok, err)
	}
}

// TestAnExpiredSessionIsNotFound: a session lasts as long as it said it would, and no longer.
func (s accountsSuite) TestAnExpiredSessionIsNotFound(t *testing.T) {
	t.Parallel()

	accounts := s.open(t)
	past := time.Now().UTC().Add(-2 * time.Hour)

	err := accounts.CreateSession(ctxFor(t), store.Session{TokenHash: "old", Actor: "local:alex", CreatedAt: past, ExpiresAt: past.Add(time.Hour)})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	for _, digest := range []string{"old", "never"} {
		_, ok, err := accounts.FindSession(ctxFor(t), digest)
		if err != nil || ok {
			t.Fatalf("FindSession(%s) = %v, %v; want not found", digest, ok, err)
		}
	}
}
