package web

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestOnlyAnAdminDisconnectsSomebodyElse: a person manages their own agents; reaching into another's is an admin's to do, and the everyone view is theirs alone.
func TestOnlyAnAdminDisconnectsSomebodyElse(t *testing.T) {
	t.Parallel()

	roles, err := CompileRoles([]string{`admin=UserID == "bob"`})
	if err != nil {
		t.Fatal(err)
	}

	server, accounts := exportServer(t, WithRoles(roles))
	connect(t, server, accounts)

	mine := html.UnescapeString(serve(t, server, http.MethodGet, "/connections", "", "alex").Body.String())
	if !strings.Contains(mine, "ops / support") || strings.Contains(mine, "everyone") {
		t.Fatalf("alex's connections should list the agent and offer no everyone view:\n%s", mine)
	}

	if all := serve(t, server, http.MethodGet, "/connections?everyone=1", "", "alex").Body.String(); strings.Contains(all, "every agent anybody") {
		t.Error("a non-admin was shown everyone's connections")
	}

	form := url.Values{"client": {clientInputOf(t, mine)}, "actor": {"local:alex"}, "pipeline": {"ops"}, "export": {"support"}}.Encode()

	err = accounts.CreateSession(t.Context(), sessionFor("carol"))
	if err != nil {
		t.Fatal(err)
	}

	carolIsRefused(t, server, form)

	if code := serve(t, server, http.MethodPost, "/connections/revoke", form, "bob").Code; code != http.StatusSeeOther {
		t.Errorf("an admin revoking alex's agent = %d, want done", code)
	}

	if gone := serve(t, server, http.MethodGet, "/connections", "", "alex").Body.String(); strings.Contains(html.UnescapeString(gone), "ops / support") {
		t.Error("the admin's revoke left alex's agent connected")
	}
}

func carolIsRefused(t *testing.T, server *Server, form string) {
	t.Helper()

	if code := serve(t, server, http.MethodPost, "/connections/revoke", form, "carol").Code; code != http.StatusForbidden {
		t.Errorf("carol revoking alex's agent = %d, want refused", code)
	}

	if still := serve(t, server, http.MethodGet, "/connections", "", "alex").Body.String(); !strings.Contains(html.UnescapeString(still), "ops / support") {
		t.Error("carol's refused revoke disconnected alex's agent anyway")
	}
}

func clientInputOf(t *testing.T, page string) string {
	t.Helper()

	_, rest, found := strings.Cut(page, `name="client" value="`)
	if !found {
		t.Fatalf("no client on the page:\n%s", page)
	}

	id, _, _ := strings.Cut(rest, `"`)

	return id
}
