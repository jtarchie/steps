package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

// A 403 is a scope refusal only when it says so; one about something else (a
// project this account may not see) is the model's to read, and its body must
// reach the SDK whole either way.
func TestReadScopeChallenge(t *testing.T) {
	for _, test := range []struct {
		name     string
		header   string
		body     string
		refusal  bool
		required string
	}{
		{
			name:     "the RFC 6750 challenge",
			header:   `Bearer error="insufficient_scope", scope="items:read", error_description="needs items:read"`,
			body:     "forbidden",
			refusal:  true,
			required: "items:read",
		},
		{
			// What Metabase actually sent on 2026-11-04: no challenge, the scope in prose.
			name:     "a body that says so in words",
			body:     `Insufficient scope to call tool: "get_content". Requires "agent:content:read"; your token holds "agent:search".`,
			refusal:  true,
			required: "agent:content:read",
		},
		{
			name: "a 403 about something else",
			body: "you may not see project 12",
		},
		{
			name:   "a challenge that is not about scope",
			header: `Bearer error="invalid_token"`,
			body:   "token revoked",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(test.body))}
			if test.header != "" {
				resp.Header.Set("WWW-Authenticate", test.header)
			}

			challenge := readScopeChallenge(resp)
			if (challenge != nil) != test.refusal {
				t.Fatalf("challenge = %+v, want a refusal: %v", challenge, test.refusal)
			}

			if challenge != nil && challenge.required != test.required {
				t.Errorf("required = %q, want %q", challenge.required, test.required)
			}

			body, _ := io.ReadAll(resp.Body)
			if string(body) != test.body {
				t.Errorf("the body left for the SDK = %q, want %q", body, test.body)
			}
		})
	}
}

func TestStaleReason(t *testing.T) {
	srv := func(scopes ...string) config.MCPServer {
		return config.MCPServer{Name: "tracker", Endpoint: inspectEndpoint, Auth: config.MCPServerAuth{Type: "oauth", Scopes: scopes}}
	}

	for _, test := range []struct {
		name  string
		srv   config.MCPServer
		token TokenFile
		want  string
	}{
		{name: "asked for what the pipeline asks for", srv: srv("a"), token: TokenFile{Scopes: []string{"a"}}},
		{name: "neither side named any", srv: srv(), token: TokenFile{}},
		{
			// A refresh token is asked for, never configured, and is not a scope a resource has.
			name: "offline_access on one side only", srv: srv("a"), token: TokenFile{Scopes: []string{"a", "offline_access"}, Granted: []string{"a"}},
		},
		{
			// The Metabase case: auth.scopes added to a pipeline whose login predates it.
			name: "auth.scopes added after the login", srv: srv("a", "b"), token: TokenFile{},
			want: `auth.scopes asks for "a", "b", but this login asked for everything the server offered`,
		},
		{
			name: "a scope removed from auth.scopes", srv: srv("a"), token: TokenFile{Scopes: []string{"a", "write"}},
			want: `but this login asked for "a", "write"`,
		},
		{
			name: "asked for and not granted", srv: srv("a", "b"), token: TokenFile{Scopes: []string{"b", "a"}, Granted: []string{"a"}},
			want: `auth.scopes asks for "b", which this login was not granted (it holds "a")`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := staleReason(testPipeline, test.srv, &test.token)
			if test.want == "" && got != "" || !strings.Contains(got, test.want) {
				t.Errorf("staleReason = %q, want %q", got, test.want)
			}
		})
	}
}

// A refusal belongs to the login that earned it: the page says so until a
// login replaces it, and not a moment after.
func TestInspectTokenSaysARefusedLoginNeedsAnother(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	UseVault(fileVault{})

	srv := config.MCPServer{Name: "refused", Endpoint: inspectEndpoint, Auth: config.MCPServerAuth{Type: "oauth"}}
	first := time.Now().Add(-time.Hour)
	saveTokenFile(t, srv.Name, &TokenFile{Endpoint: inspectEndpoint, AccessToken: "t", RefreshToken: "r", LoggedIn: first})

	noteRefusal(t.Context(), testPipeline, srv, &ScopeRefusal{Server: srv.Name, Tool: "get_content", Required: "agent:content:read", oauth: true})

	got := InspectToken(t.Context(), testPipeline, srv)
	if !got.Connected || !got.Stale || !strings.Contains(got.Detail, "get_content") || !strings.Contains(got.Detail, `"agent:content:read"`) {
		t.Errorf("after a refusal: %+v, want connected but stale, naming the tool and the scope", got)
	}

	saveTokenFile(t, srv.Name, &TokenFile{Endpoint: inspectEndpoint, AccessToken: "t2", RefreshToken: "r2", LoggedIn: first.Add(time.Minute)})

	got = InspectToken(t.Context(), testPipeline, srv)
	if got.Stale || !strings.Contains(got.Detail, "renews automatically") {
		t.Errorf("after a new login: %+v, want the refusal gone with the login that earned it", got)
	}
}

// A refresh that names a scope replaces what the login was granted; one that
// names none leaves it, since omitted means unchanged.
func TestARefreshKeepsWhatTheLoginWasGranted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	UseVault(fileVault{})

	logged := time.Now().Add(-time.Hour)
	shared := &sharedToken{pipeline: testPipeline, server: "granted", file: &TokenFile{Endpoint: inspectEndpoint, Granted: []string{"a"}, LoggedIn: logged}}

	shared.persist(t.Context(), &oauth2.Token{AccessToken: "next"})

	if got := shared.file.Granted; len(got) != 1 || got[0] != "a" {
		t.Errorf("after a refresh naming no scope, granted = %v, want [a]", got)
	}

	shared.persist(t.Context(), (&oauth2.Token{AccessToken: "after"}).WithExtra(map[string]any{"scope": "b offline_access"}))

	if got := shared.file.Granted; len(got) != 1 || got[0] != "b" {
		t.Errorf("after a refresh naming b, granted = %v, want [b]", got)
	}

	if !shared.file.LoggedIn.Equal(logged) {
		t.Errorf("a refresh moved LoggedIn to %v: a refresh is not a login", shared.file.LoggedIn)
	}
}

// What only the server can say: the scopes it offers now.
func TestCheckScopesAgainstWhatTheServerOffers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	UseVault(fileVault{})

	offered := []string{"content:read", "content:write"}
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	endpoint := server.URL + "/mcp"
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": endpoint, "authorization_servers": []string{server.URL}, "scopes_supported": offered})
	})

	for _, test := range []struct {
		name       string
		configured []string
		token      TokenFile
		want       string
		needsLogin bool
	}{
		{name: "a grant narrower than what is offered", token: TokenFile{Granted: []string{"content:read"}}},
		{name: "a login that never said what it holds", token: TokenFile{}},
		{
			name: "a login holding only names the server dropped", token: TokenFile{Granted: []string{"agent:search"}},
			want: `holds "agent:search", none of which the server offers any more`, needsLogin: true,
		},
		{
			// No login fixes this, so it must not read as one that would.
			name: "auth.scopes naming a scope the server lacks", configured: []string{"content:read", "sql:run"},
			token: TokenFile{Scopes: []string{"content:read", "sql:run"}},
			want:  `auth.scopes names "sql:run", which the server does not offer`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := config.MCPServer{Name: "offers", Endpoint: endpoint, Auth: config.MCPServerAuth{Type: "oauth", Scopes: test.configured}}
			token := test.token
			token.Endpoint, token.AccessToken = endpoint, "t"
			saveTokenFile(t, srv.Name, &token)

			err := CheckScopes(t.Context(), testPipeline, srv)
			if test.want == "" {
				if err != nil {
					t.Fatalf("CheckScopes = %v, want nil", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CheckScopes = %v, want it to say %q", err, test.want)
			}

			if errors.Is(err, ErrNeedsLogin) != test.needsLogin {
				t.Errorf("errors.Is(err, ErrNeedsLogin) = %v, want %v", !test.needsLogin, test.needsLogin)
			}
		})
	}
}
