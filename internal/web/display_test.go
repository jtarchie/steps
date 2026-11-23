package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// addDisplay is alex adding a wall display from /connections, returning the one-time key the page shows.
func addDisplay(t *testing.T, server *Server, name string) string {
	t.Helper()

	rec := serve(t, server, http.MethodPost, displaysPath, url.Values{"name": {name}}.Encode(), "alex")
	if rec.Code != http.StatusOK {
		t.Fatalf("adding a display = %d:\n%s", rec.Code, rec.Body.String())
	}

	page := html.UnescapeString(rec.Body.String())

	_, rest, found := strings.Cut(page, "https://steps.example/wall?key=")
	if !found {
		t.Fatalf("the page does not show the display's address:\n%s", page)
	}

	// The answer is drawn at the form's address, which has no GET; its region polling there swapped a 405 over the list.
	if !strings.Contains(page, `hx-get="`+connectionsPath+`"`) || strings.Contains(page, `hx-get="`+displaysPath) {
		t.Errorf("the page answering the form does not refresh from %s:\n%s", connectionsPath, page)
	}

	key, _, _ := strings.Cut(rest, `"`)
	key, _, _ = strings.Cut(key, "<")

	return key
}

// onDisplay is the TV asking for target, carrying the display cookie when it has one and nothing else.
func onDisplay(t *testing.T, server *Server, target, cookie string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.Header.Set("Accept", "text/html")

	for name, values := range header {
		req.Header[name] = values
	}

	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: displayCookie, Value: cookie, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

// TestADisplayKeyOpensTheWallAndNothingElse: a TV cannot type a password and must not hold a session that could do more than look. Its key is swapped for a cookie scoped to /wall, the address bar is left without it, and it opens nothing else: no other page, and not the read server agents use.
func TestADisplayKeyOpensTheWallAndNothingElse(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)
	key := addDisplay(t, server, "office TV")

	first := onDisplay(t, server, "/wall?key="+key, "", nil)
	if first.Code != http.StatusSeeOther || first.Header().Get("Location") != wallPath {
		t.Fatalf("opening the display's address = %d to %q; want sent on to /wall without the key", first.Code, first.Header().Get("Location"))
	}

	set := first.Header().Get("Set-Cookie")
	if !strings.Contains(set, displayCookie+"="+key) || !strings.Contains(set, "Path=/wall") || !strings.Contains(set, "HttpOnly") {
		t.Fatalf("the display cookie = %q; want the key, scoped to /wall, HttpOnly", set)
	}

	if wall := onDisplay(t, server, wallPath, key, nil); wall.Code != http.StatusOK || !strings.Contains(wall.Body.String(), `id="wall"`) {
		t.Fatalf("the display showing /wall = %d:\n%s", wall.Code, wall.Body.String())
	}

	displayOpensNothingElse(t, server, key)
}

func displayOpensNothingElse(t *testing.T, server *Server, key string) {
	t.Helper()

	if poll := onDisplay(t, server, wallPath, key, http.Header{"Hx-Request": {"true"}}); poll.Code != http.StatusOK {
		t.Errorf("the wall's own refresh on a display = %d, want served", poll.Code)
	}

	if other := onDisplay(t, server, "/p/ops", key, nil); other.Code == http.StatusOK {
		t.Error("a display key opened a pipeline page")
	}

	if mcp := onDisplay(t, server, readPath, "", http.Header{"Authorization": {"Bearer " + key}}); mcp.Code != http.StatusUnauthorized {
		t.Errorf("a display key as a bearer token at /mcp = %d, want refused", mcp.Code)
	}
}

// TestAWrongDisplayKeyIsNotLetIn: a key nobody issued sets no cookie and is asked to sign in, as any stranger is.
func TestAWrongDisplayKeyIsNotLetIn(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)

	rec := onDisplay(t, server, "/wall?key=nobody-issued-this", "", nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), loginPath) || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("an unknown key = %d to %q, cookie %q; want sent to sign in with nothing set", rec.Code, rec.Header().Get("Location"), rec.Header().Get("Set-Cookie"))
	}
}

// TestRevokingADisplayTurnsItOff: the display is listed beside the person's agents and revoked the same way, and the TV is asked to sign in on its next refresh.
func TestRevokingADisplayTurnsItOff(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)
	key := addDisplay(t, server, "office TV")

	page := html.UnescapeString(serve(t, server, http.MethodGet, connectionsPath, "", "alex").Body.String())
	if !strings.Contains(page, "office TV") || !strings.Contains(page, "wall display") {
		t.Fatalf("/connections does not list the display:\n%s", page)
	}

	form := url.Values{"client": {clientInputOf(t, page)}, "kind": {string(store.GrantRead)}}.Encode()
	if code := serve(t, server, http.MethodPost, revokePath, form, "alex").Code; code != http.StatusSeeOther {
		t.Fatalf("revoking the display = %d", code)
	}

	if rec := onDisplay(t, server, wallPath, key, nil); rec.Code == http.StatusOK {
		t.Error("a revoked display still shows the wall")
	}
}

// TestADisplayIsOnlyAsGoodAsItsPerson: a display reads as the person who added it, so one whose person this daemon no longer lets in shows nothing.
func TestADisplayIsOnlyAsGoodAsItsPerson(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)
	registerDisplayClient(t, accounts, "gone")

	err := accounts.IssueToken(t.Context(), store.OAuthToken{TokenHash: tokenDigest("orphan"), Kind: store.TokenDisplay, Grant: store.Grant{Kind: store.GrantRead, ClientID: "gone", Actor: "local:dave"}, CreatedAt: time.Now(), ExpiresAt: time.Now().AddDate(1, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}

	if rec := onDisplay(t, server, wallPath, "orphan", nil); rec.Code == http.StatusOK {
		t.Error("a display added by somebody this daemon no longer knows still shows the wall")
	}
}

func registerDisplayClient(t *testing.T, accounts store.Accounts, id string) {
	t.Helper()

	err := accounts.RegisterClient(t.Context(), store.OAuthClient{ID: id, Name: "a TV", CreatedAt: time.Now()}, 0)
	if err != nil {
		t.Fatal(err)
	}
}

// TestAnAgentsTokenIsNotADisplayKey: an agent's read token is the same person's reading, but it was given to that agent, and pasted into /wall?key= it must not become a cookie that outlives it by a year.
func TestAnAgentsTokenIsNotADisplayKey(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)
	registerDisplayClient(t, accounts, "agent")

	err := accounts.IssueToken(t.Context(), store.OAuthToken{TokenHash: tokenDigest("agent-token"), Kind: store.TokenAccess, Grant: store.Grant{Kind: store.GrantRead, ClientID: "agent", Actor: "local:alex"}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	if rec := onDisplay(t, server, "/wall?key=agent-token", "", nil); rec.Header().Get("Set-Cookie") != "" {
		t.Error("an agent's access token was taken as a display key")
	}
}
