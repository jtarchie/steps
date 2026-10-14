package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestARefreshAfterTheSessionEndsIsAPlain401: a page polling itself is not a browser navigating, so it is neither sent to the sign-in form — which would be swapped into the page — nor challenged, which would have the browser prompt over it.
func TestARefreshAfterTheSessionEndsIsAPlain401(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	accounts, err := sqlite.OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	server, err := New([]*Pipeline{pipeline}, nil, WithSessions(accounts), WithLocalUsers([]LocalUser{{Name: "alex", Hash: []byte(mustHash(t, "a"))}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/p/demo", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ended", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "" || rec.Header().Get("Location") != "" {
		t.Errorf("a refresh with an ended session = %d, challenge %q, location %q; want a bare 401", rec.Code, rec.Header().Get("WWW-Authenticate"), rec.Header().Get("Location"))
	}
}

// TestTheScriptSaysWhenTheSessionEnded: no JS runtime runs here, so the script is held to the pieces the behaviour needs; Firefox against a daemon is the behavioural check.
func TestTheScriptSaysWhenTheSessionEnded(t *testing.T) {
	t.Parallel()

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`'htmx:before:response'`, `ctx.response.status !== 401`, `e.preventDefault()`, `getElementById('attention')`, `'signed out · sign in again'`, `'/login?next='`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js does not carry %s", want)
		}
	}
}

// TestASessionEndsWithItsAccount: a session, or an agent's grant, outlives the flag that admitted its person only if nothing asks; removing the account and restarting must end both.
func TestASessionEndsWithItsAccount(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)

	err := accounts.CreateSession(t.Context(), sessionFor("gone"))
	if err != nil {
		t.Fatal(err)
	}

	if rec := serve(t, server, http.MethodGet, "/p/ops", "", "gone"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), loginPath) {
		t.Errorf("a session for an account the daemon no longer holds = %d to %q, want sent to sign in", rec.Code, rec.Header().Get("Location"))
	}

	target, export, found := server.exportNamed("ops", "support")
	if !found || !server.mayUse(t.Context(), actor.Local("alex"), target, export) {
		t.Fatal("alex should be allowed the export while her account is held")
	}

	delete(server.auth.users, "alex")

	if server.mayUse(t.Context(), actor.Local("alex"), target, export) {
		t.Error("an agent kept its export after its person's account was removed")
	}
}

// TestASignInStaysOnThisServer: every way a browser reads a path as another host is sent home instead — including the tab and newline it strips before reading.
func TestASignInStaysOnThisServer(t *testing.T) {
	t.Parallel()

	for next, want := range map[string]string{
		"/p/demo":           "/p/demo",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"/\t/evil.example":  "/",
		"/\n/evil.example":  "/",
		"/\r/evil.example":  "/",
		"https://evil.test": "/",
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}
