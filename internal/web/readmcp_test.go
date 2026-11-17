package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// readTokenFor issues an access and a refresh token under a read grant, as a consent would have.
func readTokenFor(t *testing.T, accounts store.Accounts, who string) (string, string) {
	t.Helper()

	client := "client-" + who

	err := accounts.RegisterClient(t.Context(), store.OAuthClient{ID: client, Name: "agent", RedirectURIs: []string{"https://agent.example/cb"}, CreatedAt: time.Now()}, maxClients)
	if err != nil {
		t.Fatal(err)
	}

	grant := store.Grant{Kind: store.GrantRead, ClientID: client, Actor: "local:" + who}

	for _, token := range []store.OAuthToken{
		{TokenHash: tokenDigest("access-" + who), Kind: store.TokenAccess, Grant: grant, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
		{TokenHash: tokenDigest("refresh-" + who), Kind: store.TokenRefresh, Grant: grant, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
	} {
		err := accounts.IssueToken(t.Context(), token)
		if err != nil {
			t.Fatal(err)
		}
	}

	return "access-" + who, "refresh-" + who
}

func readCall(t *testing.T, server *Server, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, readPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

func refreshWith(t *testing.T, server *Server, who, refresh string) *httptest.ResponseRecorder {
	t.Helper()

	return serve(t, server, http.MethodPost, tokenPath, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {"client-" + who}}.Encode(), "")
}

// Whether a person may read is asked on every call and every refresh, from the daemon's record: carol has no viewer role, so a token she holds is refused at once and cannot be renewed, while bob's works.
func TestReadAccessIsAskedOnEveryCallAndRefresh(t *testing.T) {
	t.Parallel()

	roles, err := CompileRoles([]string{`admin=UserID == "alex"`, `viewer=UserID == "alex" || UserID == "bob"`})
	if err != nil {
		t.Fatal(err)
	}

	server, accounts := exportServer(t, WithRoles(roles))

	bobAccess, bobRefresh := readTokenFor(t, accounts, "bob")
	carolAccess, carolRefresh := readTokenFor(t, accounts, "carol")

	if rec := readCall(t, server, bobAccess); rec.Code != http.StatusOK {
		t.Errorf("bob's read = %d %s", rec.Code, rec.Body.String())
	}

	if rec := readCall(t, server, carolAccess); rec.Code != http.StatusForbidden {
		t.Errorf("carol's read with no viewer role = %d, want 403", rec.Code)
	}

	if rec := refreshWith(t, server, "bob", bobRefresh); rec.Code != http.StatusOK {
		t.Errorf("bob's refresh = %d %s", rec.Code, rec.Body.String())
	}

	if rec := refreshWith(t, server, "carol", carolRefresh); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Errorf("carol's refresh = %d %s, want invalid_grant", rec.Code, rec.Body.String())
	}
}

// --mcp-access is asked on every call too, apart from the role: bob may view, and still not read through an agent.
func TestMCPAccessIsAskedOnEveryCall(t *testing.T) {
	t.Parallel()

	rule, err := CompileMCPAccess(`UserID == "alex"`)
	if err != nil {
		t.Fatal(err)
	}

	server, accounts := exportServer(t, WithMCPAccess(rule))

	alex, _ := readTokenFor(t, accounts, "alex")
	bob, bobRefresh := readTokenFor(t, accounts, "bob")

	if rec := readCall(t, server, alex); rec.Code != http.StatusOK {
		t.Errorf("alex's read = %d", rec.Code)
	}

	if rec := readCall(t, server, bob); rec.Code != http.StatusForbidden {
		t.Errorf("bob's read outside --mcp-access = %d, want 403", rec.Code)
	}

	if rec := refreshWith(t, server, "bob", bobRefresh); rec.Code != http.StatusBadRequest {
		t.Errorf("bob's refresh outside --mcp-access = %d, want refused", rec.Code)
	}
}

// With no token the read server says where to get one: the protected-resource metadata, which names the authorization server.
func TestAnUnauthenticatedReadIsToldWhereToSignIn(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)

	rec := readCall(t, server, "")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `resource_metadata="https://steps.example/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("no token = %d, %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	meta := serve(t, server, http.MethodGet, readResourceMetaPath, "", "")
	if meta.Code != http.StatusOK || !strings.Contains(meta.Body.String(), `"resource":"https://steps.example/mcp"`) {
		t.Errorf("metadata = %d %s", meta.Code, meta.Body.String())
	}
}

// A daemon with only the Basic prompt cannot issue a read token, so it serves no read server rather than one an agent would need a password for.
func TestABasicOnlyDaemonServesNoReads(t *testing.T) {
	t.Parallel()

	server, err := New(nil, nil, WithBasicAuth("alex", "a"))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, readPath, strings.NewReader(`{}`))
	req.SetBasicAuth("alex", "a")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /mcp on a Basic-only daemon = %d, want it not served", rec.Code)
	}
}

// --mcp-access on a daemon nobody signs in to would decide about nobody, so the daemon refuses to start with it.
func TestMCPAccessNeedsSomebodyToSignIn(t *testing.T) {
	t.Parallel()

	rule, err := CompileMCPAccess("true")
	if err != nil {
		t.Fatal(err)
	}

	_, err = New(nil, nil, WithMCPAccess(rule))
	if err == nil || !strings.Contains(err.Error(), "nobody can sign in") {
		t.Errorf("New with --mcp-access and no sign-in = %v", err)
	}
}
