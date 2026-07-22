package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
)

// fakeOAuthServer wires up a full, minimal MCP + OAuth authorization server
// on one httptest.Server, enough to drive Login() end-to-end: protected
// resource metadata, authorization server metadata (advertising PKCE
// support, as GetAuthServerMeta requires), dynamic client registration, and
// a token endpoint that always succeeds. The MCP endpoint itself requires a
// bearer token matching what the token endpoint issues, so a successful
// Login is provable by a subsequent authenticated ListTools call.
type fakeOAuthServer struct {
	mux         *http.ServeMux
	server      *httptest.Server
	issuedToken string
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()

	f := &fakeOAuthServer{mux: http.NewServeMux(), issuedToken: "issued-access-token"}
	f.server = httptest.NewServer(f.mux)
	t.Cleanup(f.server.Close)

	base := f.server.URL

	mcpHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return echoServer() }, nil)
	f.mux.Handle("/mcp", requireBearer(f.issuedToken, mcpHandler))

	f.mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              base + "/mcp",
			"authorization_servers": []string{base},
		})
	})

	f.mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"registration_endpoint":                 base + "/register",
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})

	f.mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any

		_ = json.NewDecoder(r.Body).Decode(&meta)

		resp := map[string]any{
			"client_id":     "test-client-id",
			"redirect_uris": meta["redirect_uris"],
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	f.mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": f.issuedToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	return f
}

// fakeBrowserOpen stands in for a human: it extracts the redirect_uri and
// state from the authorization URL steps built and hits the loopback
// callback directly with a fake code, exactly as a real browser would after
// a user completed a real authorization page.
func fakeBrowserOpen(t *testing.T) func(string) error {
	t.Helper()

	return func(rawURL string) error {
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("fake browser: parse authorization URL: %w", err)
		}

		q := u.Query()

		callbackURL := q.Get("redirect_uri") + "?code=fake-code&state=" + q.Get("state")

		resp, err := http.Get(callbackURL) //nolint:gosec,noctx // test-only, fixed loopback URL built above
		if err != nil {
			return fmt.Errorf("fake browser: visit callback URL: %w", err)
		}

		return resp.Body.Close()
	}
}

func TestLoginEndToEnd(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{
		Name:     "fake",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Login(ctx, srv, fakeBrowserOpen(t))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	path, err := TokenPath(srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}

	assertLoginPersisted(t, tf, srv, fake.issuedToken)
	assertPersistedTokenWorks(t, ctx, srv)
}

// assertLoginPersisted checks the token file Login wrote.
func assertLoginPersisted(t *testing.T, tf *TokenFile, srv config.MCPServer, wantAccessToken string) {
	t.Helper()

	if tf.AccessToken != wantAccessToken {
		t.Errorf("persisted AccessToken = %q, want %q", tf.AccessToken, wantAccessToken)
	}

	if tf.Endpoint != srv.Endpoint {
		t.Errorf("persisted Endpoint = %q, want %q", tf.Endpoint, srv.Endpoint)
	}

	if tf.ClientID != "test-client-id" {
		t.Errorf("persisted ClientID = %q, want test-client-id", tf.ClientID)
	}
}

// assertPersistedTokenWorks confirms the persisted token actually
// authenticates against the real server via the normal (non-login)
// headless path.
func assertPersistedTokenWorks(t *testing.T, ctx context.Context, srv config.MCPServer) { //nolint:revive // t before ctx matches this file's other test-helper signatures
	t.Helper()

	client, err := Connect(ctx, srv)
	if err != nil {
		t.Fatalf("Connect after Login: %v", err)
	}
	defer func() { _ = client.Close() }()

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools after Login: %v", err)
	}

	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools after Login = %+v", tools)
	}
}
