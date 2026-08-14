package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
//
// challenge is likewise read per request: it is the WWW-Authenticate value
// the unauthorized MCP endpoint answers with, which real servers do not all
// spell the same way.
//
// issuesRefreshToken models the axis this package cares most about: a real
// authorization server hands back a refresh token only when the client
// registered for the refresh_token grant, and a client that did not gets an
// access token that quietly dies at its first expiry. Flipping it off is how
// a test reproduces that server without a browser.
//
// registration captures what was sent to /register, so a test can assert on
// the grant types steps declares rather than on their downstream effect.
type fakeOAuthServer struct {
	mux                  *http.ServeMux
	server               *httptest.Server
	issuedToken          string
	supportsRegistration bool
	issuesRefreshToken   bool
	challenge            string
	registration         map[string]any
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()

	f := &fakeOAuthServer{
		mux:                  http.NewServeMux(),
		issuedToken:          "issued-access-token",
		supportsRegistration: true,
		issuesRefreshToken:   true,
		challenge:            "Bearer",
	}
	f.server = httptest.NewServer(f.mux)
	t.Cleanup(f.server.Close)

	base := f.server.URL

	mcpHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return echoServer() }, nil)
	f.mux.Handle("/mcp", requireBearer(f.issuedToken, func() string { return f.challenge }, mcpHandler))

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

		f.registration = meta

		resp := map[string]any{
			"client_id":     "test-client-id",
			"redirect_uris": meta["redirect_uris"],
			"grant_types":   meta["grant_types"],
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	f.mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"access_token": f.issuedToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		}

		if f.issuesRefreshToken {
			body["refresh_token"] = "issued-refresh-token"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
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

// capturingBrowserOpen is fakeBrowserOpen plus a record of the authorization
// URL's query, for tests that assert on what steps ASKED the authorization
// server for rather than on what it handed back.
func capturingBrowserOpen(t *testing.T, into *url.Values) func(string) error {
	t.Helper()

	inner := fakeBrowserOpen(t)

	return func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err == nil {
			*into = parsed.Query()
		}

		return inner(rawURL)
	}
}

// TestLoginRegistersRefreshGrant pins the field whose absence caused a
// working login to expire unattended an hour later: RFC 7591 §2 defaults an
// omitted grant_types to authorization_code alone, so a client that does not
// declare refresh_token has asked not to be given a refresh token, and a
// conforming server obliges.
//
// Asserted on the registration request rather than on the resulting token
// because that is where the mistake lives — a fake that hands out refresh
// tokens regardless would pass on the token alone while the real server this
// was found against would not.
func TestLoginRegistersRefreshGrant(t *testing.T) {
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

	grants, _ := fake.registration["grant_types"].([]any)

	var got []string

	for _, grant := range grants {
		if name, ok := grant.(string); ok {
			got = append(got, name)
		}
	}

	if !slices.Contains(got, "refresh_token") || !slices.Contains(got, "authorization_code") {
		t.Fatalf("registered grant_types = %v, want both authorization_code and refresh_token", got)
	}

	path, err := TokenPath(srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}

	if tf.RefreshToken == "" {
		t.Fatal("persisted RefreshToken is empty; nothing could renew this credential")
	}
}

// TestLoginRequestsConfiguredScopes covers auth.scopes: actually reaching the
// authorization server. It used to be persisted and read by nothing, so a
// pipeline asking for one scope was granted every scope the server
// advertised — least privilege that was written down and never sent.
func TestLoginRequestsConfiguredScopes(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{
		Name:     "scoped",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth", Scopes: []string{"agent:search", "agent:query:execute"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var asked url.Values

	err := Login(ctx, srv, capturingBrowserOpen(t, &asked))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if got := asked.Get("scope"); got != "agent:search agent:query:execute" {
		t.Fatalf("authorization request scope = %q, want the two configured scopes", got)
	}
}

// TestLoginWithoutConfiguredScopesAsksForWhatIsOffered is the other half:
// an empty auth.scopes: must leave the SDK's choice alone rather than
// narrowing to nothing, since asking for everything on offer is the right
// default for a server the pipeline has no opinion about.
func TestLoginWithoutConfiguredScopesAsksForWhatIsOffered(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{
		Name:     "unscoped",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var asked url.Values

	err := Login(ctx, srv, capturingBrowserOpen(t, &asked))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, present := asked["scope"]; present && asked.Get("scope") == "" {
		t.Fatal("authorization request sent an empty scope; it should have been left untouched")
	}
}

// TestLoginFailsWhenNoRefreshTokenIssued is the incident, reproduced: a
// server that authorizes happily and returns an access token with no refresh
// token beside it. Login must not report success — that credential works for
// an hour and then fails in a watcher with nobody watching.
//
// The token is still expected on disk. It is valid, and a `steps run` right
// now works with it; discarding a working credential to signal a future
// problem would be the worse trade.
func TestLoginFailsWhenNoRefreshTokenIssued(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)
	fake.issuesRefreshToken = false

	srv := config.MCPServer{
		Name:     "norefresh",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Login(ctx, srv, fakeBrowserOpen(t))
	if err == nil {
		t.Fatal("Login reported success for a credential that cannot be renewed")
	}

	if !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("Login error does not name the cause: %v", err)
	}

	path, err := TokenPath(srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: the working token should still have been saved: %v", err)
	}

	if tf.AccessToken != fake.issuedToken {
		t.Fatalf("persisted AccessToken = %q, want %q", tf.AccessToken, fake.issuedToken)
	}
}

// TestLoginKeepsARenewableTokenOverADisposableOne is the case the test above
// does not cover: the same failure, but with something already on disk worth
// more than what the login just obtained.
//
// Login persists before it judges, deliberately — writing a token that works
// now beats discarding it because it will not last. That reasoning holds
// against an empty directory and inverts against a renewable credential:
// re-running login to widen scopes, against a server that this time issues no
// refresh token, would replace a credential that survives unattended with one
// that dies at expiry, and the old one is not recoverable.
func TestLoginKeepsARenewableTokenOverADisposableOne(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)
	fake.issuesRefreshToken = false

	srv := config.MCPServer{
		Name:     "downgrade",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	path, err := TokenPath(srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	existing := &TokenFile{
		Endpoint:     srv.Endpoint,
		ClientID:     "client-id",
		TokenURL:     fake.server.URL + "/token",
		AccessToken:  "previous-access-token",
		RefreshToken: "previous-refresh-token",
		Expiry:       time.Now().Add(time.Hour),
	}

	err = existing.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = Login(ctx, srv, fakeBrowserOpen(t))
	if err == nil {
		t.Fatal("Login replaced a renewable credential with one that cannot be renewed, and reported success")
	}

	kept, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}

	if kept.RefreshToken != existing.RefreshToken {
		t.Fatalf("persisted RefreshToken = %q, want the previous %q to have been kept", kept.RefreshToken, existing.RefreshToken)
	}

	if kept.AccessToken != existing.AccessToken {
		t.Fatalf("persisted AccessToken = %q, want the previous %q to have been kept", kept.AccessToken, existing.AccessToken)
	}
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

// TestLoginSpaceSeparatedChallenge is the Metabase case: a server whose 401
// separates WWW-Authenticate auth-params with spaces instead of commas.
// The SDK's strict RFC 9110 parser rejects that header outright, which used
// to abort login before the browser opened; see challenge.go.
func TestLoginSpaceSeparatedChallenge(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)
	fake.challenge = fmt.Sprintf(
		`Bearer realm="mcp" resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`,
		fake.server.URL,
	)

	srv := config.MCPServer{
		Name:     "spacey",
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

	if tf.AccessToken != fake.issuedToken {
		t.Errorf("persisted AccessToken = %q, want %q", tf.AccessToken, fake.issuedToken)
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

func issBrowserOpen(t *testing.T, iss string) func(string) error {
	t.Helper()

	return func(rawURL string) error {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("fake browser: parse authorization URL: %w", err)
		}

		query := parsed.Query()

		callbackURL := fmt.Sprintf("%s?code=fake-code&state=%s&iss=%s",
			query.Get("redirect_uri"), url.QueryEscape(query.Get("state")), url.QueryEscape(iss))

		resp, err := http.Get(callbackURL) //nolint:gosec,noctx // test-only, loopback URL built from this flow's own redirect_uri
		if err != nil {
			return fmt.Errorf("fake browser: visit callback URL: %w", err)
		}

		return resp.Body.Close()
	}
}

// TestLoginAcceptsAnUnadvertisedIss is the second Metabase case, after the
// space-separated challenge: an authorization server that returns iss on
// every redirect and never advertises
// authorization_response_iss_parameter_supported.
//
// The SDK rejects that pairing outright, so forwarding iss verbatim made
// login impossible against a server doing MORE than it promised rather than
// less. steps validates the value itself and then withholds it from a check
// that would only refuse it.
func TestLoginAcceptsAnUnadvertisedIss(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{
		Name:     "unadvertised-iss",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The fake's metadata advertises no iss support, and its issuer is its
	// own base URL — exactly Metabase's shape.
	err := Login(ctx, srv, issBrowserOpen(t, fake.server.URL))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	assertPersistedTokenWorks(t, ctx, srv)
}

// TestLoginRejectsAMismatchedIss is what the check above must not cost. An
// iss naming a DIFFERENT issuer than the one this flow was started against
// is the mix-up attack RFC 9207 exists to catch, and it has to fail whether
// or not the server advertised the parameter — otherwise "tolerate an
// unadvertised iss" would quietly mean "ignore iss".
func TestLoginRejectsAMismatchedIss(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{
		Name:     "mixed-up",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := Login(ctx, srv, issBrowserOpen(t, "https://attacker.example.com"))
	if err == nil {
		t.Fatal("Login: exchanged a code against an issuer it never started a flow with")
	}

	if !strings.Contains(err.Error(), "attacker.example.com") {
		t.Fatalf("Login error does not name the unexpected issuer: %v", err)
	}
}

// TestLoginHonoursCallbackPort proves the redirect URI is predictable when
// auth.callback_port is set, which is the entire point of the field: an
// authorization server that validates redirect URIs exactly cannot be
// pre-registered against a port the kernel picks fresh every run.
func TestLoginHonoursCallbackPort(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	// Borrow a port the OS says is free, release it, then require Login to
	// bind that exact one. Asking for a hardcoded port would flake against
	// whatever else is running on the machine.
	var lc net.ListenConfig

	probe, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}

	port := probe.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // net.Listen("tcp", ...) always yields a *net.TCPAddr

	err = probe.Close()
	if err != nil {
		t.Fatalf("probe close: %v", err)
	}

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{
		Name:     "pinned",
		Endpoint: fake.server.URL + "/mcp",
		Auth:     config.MCPServerAuth{Type: "oauth", CallbackPort: port},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var query url.Values

	err = Login(ctx, srv, capturingBrowserOpen(t, &query))
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	want := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	if got := query.Get("redirect_uri"); got != want {
		t.Fatalf("redirect_uri = %q, want %q", got, want)
	}

	// The registration has to name the same URI, or a server that stores it
	// would reject the very redirect this pinning exists to make matchable.
	uris, _ := fake.registration["redirect_uris"].([]any)
	if len(uris) != 1 || uris[0] != want {
		t.Fatalf("registered redirect_uris = %v, want exactly [%q]", uris, want)
	}
}

// TestResolveIss covers the cases the two Login tests above cannot reach,
// because both run against a fake whose two discoveries agree.
//
// The interesting axis is issAdvertised. It used to select whether the issuer
// was compared AT ALL — an advertised flag handed the decision to the SDK,
// which checks against the authorization server IT resolved rather than the
// one this flow registered a client with. Those are the same server right up
// until the moment it matters.
func TestResolveIss(t *testing.T) {
	t.Parallel()

	const issuer = "https://as.example"

	tests := []struct {
		name      string
		req       authRequest
		got       string
		want      string
		wantError bool
	}{{
		name: "advertised and matching is forwarded for the SDK to check too",
		req:  authRequest{issuer: issuer, issAdvertised: true},
		got:  issuer,
		want: issuer,
	}, {
		// Previously returned the attacker's iss unexamined, on the assumption
		// the SDK held the same expected issuer.
		name:      "advertised and mismatched is refused here",
		req:       authRequest{issuer: issuer, issAdvertised: true},
		got:       "https://attacker.example",
		wantError: true,
	}, {
		name: "unadvertised and matching is verified then withheld",
		req:  authRequest{issuer: issuer},
		got:  issuer,
		want: "",
	}, {
		name:      "unadvertised and mismatched is refused",
		req:       authRequest{issuer: issuer},
		got:       "https://attacker.example",
		wantError: true,
	}, {
		name: "absent iss stays absent",
		req:  authRequest{issuer: issuer},
		got:  "",
		want: "",
	}, {
		// RFC 8414 treats these as the same server; an exact compare failed
		// the login outright and blamed the server for a mix-up attack.
		name: "trailing slash on the discovered issuer is not a mix-up",
		req:  authRequest{issuer: issuer + "/"},
		got:  issuer,
		want: "",
	}, {
		name: "trailing slash on the returned iss is not a mix-up",
		req:  authRequest{issuer: issuer, issAdvertised: true},
		got:  issuer + "/",
		want: issuer + "/",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := test.req.resolveIss(test.got)
			if test.wantError {
				if err == nil {
					t.Fatalf("resolveIss(%q) = %q, want an error", test.got, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveIss(%q): %v", test.got, err)
			}

			if got != test.want {
				t.Fatalf("resolveIss(%q) = %q, want %q", test.got, got, test.want)
			}
		})
	}
}
