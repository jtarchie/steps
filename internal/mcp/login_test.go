package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
//
// supportsRegistration is read per request, so a test can flip it off to
// model a server like Slack's that requires a pre-registered client: the
// metadata then advertises no registration_endpoint and /register refuses.
type fakeOAuthServer struct {
	mux                  *http.ServeMux
	server               *httptest.Server
	issuedToken          string
	supportsRegistration bool
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()

	f := &fakeOAuthServer{mux: http.NewServeMux(), issuedToken: "issued-access-token", supportsRegistration: true}
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
		meta := map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		}

		if f.supportsRegistration {
			meta["registration_endpoint"] = base + "/register"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	})

	f.mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if !f.supportsRegistration {
			http.Error(w, "dynamic client registration is not supported", http.StatusNotFound)

			return
		}

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

// TestLoginPreregisteredClientWithoutDCR is the Slack case: an
// authorization server advertising no registration_endpoint, where login
// must proceed on the client_id the config supplies rather than failing
// during discovery.
func TestLoginPreregisteredClientWithoutDCR(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TEST_MCP_CLIENT_SECRET", "registered-client-secret")

	fake := newFakeOAuthServer(t)
	fake.supportsRegistration = false

	srv := config.MCPServer{
		Name:     "preregistered",
		Endpoint: fake.server.URL + "/mcp",
		Auth: config.MCPServerAuth{ //nolint:gosec // G101: client_secret_env names an env var, it is not a credential
			Type:            "oauth",
			ClientID:        "1234567890.9876543210",
			ClientSecretEnv: "TEST_MCP_CLIENT_SECRET",
		},
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

	if tf.AccessToken != fake.issuedToken {
		t.Errorf("persisted AccessToken = %q, want %q", tf.AccessToken, fake.issuedToken)
	}

	// The configured credentials are what got persisted, so a later
	// headless refresh reuses the registered app rather than re-registering.
	if tf.ClientID != srv.Auth.ClientID {
		t.Errorf("persisted ClientID = %q, want %q", tf.ClientID, srv.Auth.ClientID)
	}

	if tf.ClientSecret != "registered-client-secret" {
		t.Errorf("persisted ClientSecret = %q, want the value of $TEST_MCP_CLIENT_SECRET", tf.ClientSecret)
	}

	assertPersistedTokenWorks(t, ctx, srv)
}

// TestLoginWithoutDCRNorClientIDNamesTheFix checks the error a server with
// no registration endpoint produces when nothing was pre-registered: it has
// to name client_id, since that is the only way forward.
func TestLoginWithoutDCRNorClientIDNamesTheFix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)
	fake.supportsRegistration = false

	srv := config.MCPServer{
		Name:     "preregistered",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Login(ctx, srv, fakeBrowserOpen(t))
	if err == nil {
		t.Fatal("Login succeeded against a server with no registration endpoint")
	}

	if !strings.Contains(err.Error(), "auth.client_id") {
		t.Errorf("error does not name the fix: %v", err)
	}
}

// TestLoginPreregisteredSecretEnvUnset fails before any browser opens when
// client_secret_env names a variable that isn't set — the alternative is a
// public-client exchange the server rejects as an opaque 401.
func TestLoginPreregisteredSecretEnvUnset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TEST_MCP_CLIENT_SECRET", "")

	fake := newFakeOAuthServer(t)
	fake.supportsRegistration = false

	srv := config.MCPServer{
		Name:     "preregistered",
		Endpoint: fake.server.URL + "/mcp",
		Auth: config.MCPServerAuth{ //nolint:gosec // G101: client_secret_env names an env var, it is not a credential
			Type:            "oauth",
			ClientID:        "1234567890.9876543210",
			ClientSecretEnv: "TEST_MCP_CLIENT_SECRET",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Login(ctx, srv, func(string) error {
		t.Error("browser opened despite an unset client secret")

		return nil
	})
	if err == nil {
		t.Fatal("Login succeeded with an unset client_secret_env")
	}

	if !strings.Contains(err.Error(), "TEST_MCP_CLIENT_SECRET") {
		t.Errorf("error does not name the unset variable: %v", err)
	}
}
