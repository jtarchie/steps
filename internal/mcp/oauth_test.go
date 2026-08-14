package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

func TestTokenFileSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "linear.json")

	want := &TokenFile{ //nolint:gosec // test fixture literals, not real credentials
		Endpoint:     "https://mcp.linear.app/mcp",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		TokenURL:     "https://linear.app/oauth/token",
		Scopes:       []string{"read"},
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour).UTC().Round(time.Second),
	}

	err := want.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}

	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 0700", perm)
	}

	got, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}

	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.ClientID != want.ClientID {
		t.Errorf("LoadTokenFile = %+v, want %+v", got, want)
	}
}

func TestLoadTokenFileMissing(t *testing.T) {
	t.Parallel()

	_, err := LoadTokenFile(filepath.Join(t.TempDir(), "missing.json"))
	if err == nil {
		t.Fatal("LoadTokenFile: expected an error for a missing file")
	}
}

// tokenEndpointServer builds an httptest.Server acting as an OAuth token
// endpoint: it always grants a refresh, returning a fresh access_token and
// (to exercise the write-back path this test exists for) a NEW
// refresh_token each time — mirroring a provider that rotates refresh
// tokens on every use, which is exactly the case a non-persisting token
// source would break under on a long-running steps watch.
func tokenEndpointServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()

	calls := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token-call-" + strconv.Itoa(calls),
			"refresh_token": "refresh-token-call-" + strconv.Itoa(calls),
			"token_type":    "Bearer",
			"expires_in":    1, // already-expired-ish, forces a refresh on the next Token() call
		})
	}))

	t.Cleanup(ts.Close)

	return ts, &calls
}

func TestOAuthTokenSourceRefreshesAndPersists(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	ts, calls := tokenEndpointServer(t)

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir) // covers both darwin ($HOME-based) and linux (XDG-based) os.UserConfigDir()
	path, err := TokenPath("linear")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	tf := &TokenFile{
		Endpoint:     srv.Endpoint,
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		TokenURL:     ts.URL,
		AccessToken:  "stale-access-token",
		RefreshToken: "stale-refresh-token",
		Expiry:       time.Now().Add(-time.Hour), // already expired, forces an immediate refresh
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	source, err := oauthTokenSource(context.Background(), srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	refreshAndCheck(t, source, "access-token-call-1")

	// The refreshed (rotated) token must have been written back to disk —
	// this is the regression this test exists to catch: without write-back,
	// the persisted refresh_token stays "stale-refresh-token" forever, and a
	// SECOND refresh attempt (simulated below) would be sent with a
	// refresh_token the provider already invalidated.
	assertPersistedRotatedToken(t, path, ts.URL)

	// A second, independent token source (as a fresh `steps watch` process
	// would construct) must pick up the rotated refresh_token from disk, not
	// the original stale one — proving persistence survives across
	// "process restarts" (a fresh source here stands in for that).
	source2, err := oauthTokenSource(context.Background(), srv)
	if err != nil {
		t.Fatalf("oauthTokenSource (2nd): %v", err)
	}

	refreshAndCheck(t, source2, "access-token-call-2")

	if *calls != 2 {
		t.Fatalf("token endpoint called %d times, want 2", *calls)
	}
}

// refreshAndCheck calls source.Token() and fails the test unless it returns
// wantAccessToken.
func refreshAndCheck(t *testing.T, source oauth2.TokenSource, wantAccessToken string) *oauth2.Token {
	t.Helper()

	tok, err := source.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if tok.AccessToken != wantAccessToken {
		t.Fatalf("access token = %q, want %q", tok.AccessToken, wantAccessToken)
	}

	return tok
}

// assertPersistedRotatedToken checks that the token file at path now holds
// the rotated call-1 tokens (and hasn't lost its client credentials).
func assertPersistedRotatedToken(t *testing.T, path, wantTokenURL string) {
	t.Helper()

	onDisk, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile after 1st refresh: %v", err)
	}

	if onDisk.AccessToken != "access-token-call-1" || onDisk.RefreshToken != "refresh-token-call-1" {
		t.Fatalf("persisted token = %+v, want the rotated call-1 tokens", onDisk)
	}

	if onDisk.ClientID != "client-id" || onDisk.TokenURL != wantTokenURL {
		t.Errorf("persisted token lost client_id/token_url: %+v", onDisk)
	}
}

func TestOAuthTokenSourceMissingFile(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := config.MCPServer{Name: "ghost-server", Endpoint: "https://example.com/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	_, err := oauthTokenSource(context.Background(), srv)
	if err == nil {
		t.Fatal("oauthTokenSource: expected an error when no token file exists")
	}
}

func TestOAuthTokenSourceEndpointMismatch(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	path, err := TokenPath(srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf := &TokenFile{Endpoint: "https://a-different-endpoint.example.com/mcp", AccessToken: "x"}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = oauthTokenSource(context.Background(), srv)
	if err == nil {
		t.Fatal("oauthTokenSource: expected an error for a persisted-endpoint mismatch")
	}
}

// TestOAuthTokenSourceNeedsLoginWhenExpiredWithNoRefreshToken is the state
// the incident ended in: a token file holding an access token that expired
// an hour ago and no refresh token to trade for a new one.
//
// x/oauth2 answers this with "token expired and refresh token is not set",
// which is true and names neither the server nor the fix, and only after a
// transport has been built. It has to be ErrNeedsLogin, because that is what
// tells `steps watch` this is not worth waiting out.
func TestOAuthTokenSourceNeedsLoginWhenExpiredWithNoRefreshToken(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := TokenPath("metabase")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	srv := config.MCPServer{
		Name:     "metabase",
		Endpoint: "https://example.invalid/api/metabase-mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	tf := &TokenFile{ //nolint:gosec // test fixture literals, not real credentials
		Endpoint:    srv.Endpoint,
		ClientID:    "client-id",
		TokenURL:    "https://example.invalid/oauth/token",
		AccessToken: "expired-access-token",
		Expiry:      time.Now().Add(-time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = oauthTokenSource(context.Background(), srv)
	if err == nil {
		t.Fatal("oauthTokenSource: want an error for an expired, unrefreshable token")
	}

	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("error = %v, want it to wrap ErrNeedsLogin so watch treats it as terminal", err)
	}
}

// TestOAuthTokenSourceUnexpiredWithNoRefreshTokenStillWorks is the other
// side of that check. A token that has not run out is usable right now,
// which is all a `steps run` needs — refusing it would fail a run that would
// have succeeded. Whether it can survive unattended is Login's question.
func TestOAuthTokenSourceUnexpiredWithNoRefreshTokenStillWorks(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := TokenPath("metabase")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	srv := config.MCPServer{
		Name:     "metabase",
		Endpoint: "https://example.invalid/api/metabase-mcp",
		Auth:     config.MCPServerAuth{Type: "oauth"},
	}

	tf := &TokenFile{ //nolint:gosec // test fixture literals, not real credentials
		Endpoint:    srv.Endpoint,
		ClientID:    "client-id",
		TokenURL:    "https://example.invalid/oauth/token",
		AccessToken: "live-access-token",
		Expiry:      time.Now().Add(time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	source, err := oauthTokenSource(context.Background(), srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	tok, err := source.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if tok.AccessToken != "live-access-token" {
		t.Fatalf("AccessToken = %q, want the unexpired token to be handed back as-is", tok.AccessToken)
	}
}

// TestOAuthTokenSourceNeedsLoginWhenRefreshRejected covers the second way a
// credential dies for good: the refresh token is present but the
// authorization server refuses it (revoked, expired, or already rotated away
// by another process). The server ANSWERING and saying no is not something a
// later poll improves.
func TestOAuthTokenSourceNeedsLoginWhenRefreshRejected(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := TokenPath("linear")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	tf := &TokenFile{
		Endpoint:     srv.Endpoint,
		ClientID:     "client-id",
		TokenURL:     ts.URL,
		AccessToken:  "stale-access-token",
		RefreshToken: "revoked-refresh-token",
		Expiry:       time.Now().Add(-time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	source, err := oauthTokenSource(context.Background(), srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	_, err = source.Token()
	if err == nil {
		t.Fatal("Token: want an error when the authorization server rejects the refresh")
	}

	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("error = %v, want it to wrap ErrNeedsLogin", err)
	}
}

// TestOAuthTokenSourceKeepsNetworkFailureWaitable is the boundary the two
// tests above share an edge with. A token endpoint that cannot be reached
// says nothing about the credential, and marking it ErrNeedsLogin would make
// a watcher exit over a blip that heals on its own — the failure mode
// config.Problem.Transient exists to prevent.
func TestOAuthTokenSourceKeepsNetworkFailureWaitable(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := TokenPath("linear")
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	tf := &TokenFile{ //nolint:gosec // test fixture literals, not real credentials
		Endpoint:     srv.Endpoint,
		ClientID:     "client-id",
		TokenURL:     "http://127.0.0.1:1/oauth/token", // nothing listens here
		AccessToken:  "stale-access-token",
		RefreshToken: "good-refresh-token",
		Expiry:       time.Now().Add(-time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	source, err := oauthTokenSource(context.Background(), srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	_, err = source.Token()
	if err == nil {
		t.Fatal("Token: want an error when the token endpoint is unreachable")
	}

	if errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("error = %v, want an unreachable endpoint to stay waitable, not demand a re-login", err)
	}
}
