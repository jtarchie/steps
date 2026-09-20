package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// The hosted front door has to reach the SAME persisted, working token the loopback one does — the refactor that made room for it moved the flow's body, and a body that silently stopped persisting would pass every test of the callback alone.
func TestLoginHostedEndToEnd(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv (via TokenPath's os.UserConfigDir()).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	fake := newFakeOAuthServer(t)

	srv := config.MCPServer{Name: "fake", Endpoint: fake.server.URL + "/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}

	announced := make(chan string, 1)
	daemon, hosted := hostedOnADaemon(t, func(authURL string) { announced <- authURL })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- LoginHosted(ctx, srv, hosted) }()

	var authURL string

	select {
	case authURL = <-announced:
	case err := <-done:
		t.Fatalf("LoginHosted ended before announcing a URL: %v", err)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}

	if got := parsed.Query().Get("redirect_uri"); got != daemon.URL+"/mcp/callback" {
		t.Errorf("redirect_uri = %q, want the hosted address — a loopback one sends the browser to a machine with nothing listening", got)
	}

	// A stranger's request must not settle the flow, which is what Matches is for.
	if hosted.Matches("not-the-state") || hosted.Matches("") {
		t.Error("the callback accepted a state it never minted")
	}

	err = fakeBrowserOpen(t)(authURL)
	if err != nil {
		t.Fatal(err)
	}

	err = <-done
	if err != nil {
		t.Fatalf("LoginHosted: %v", err)
	}

	assertHostedLoginPersisted(t, ctx, srv, fake.issuedToken)

	// Single use: the state that just settled a login is worth nothing afterwards.
	if state := parsed.Query().Get("state"); replay(t, daemon.URL, state) == http.StatusOK {
		t.Error("a replayed redirect was accepted as a second answer")
	}
}

// hostedOnADaemon is somebody else's server routing the redirect to the callback, exactly as the daemon's /mcp/callback does.
func hostedOnADaemon(t *testing.T, announce func(string)) (*httptest.Server, *HostedCallback) {
	t.Helper()

	var hosted *HostedCallback

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosted.Matches(r.URL.Query().Get("state")) {
			http.NotFound(w, r)

			return
		}

		hosted.ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)

	hosted = NewHostedCallback(daemon.URL+"/mcp/callback", announce)

	return daemon, hosted
}

func assertHostedLoginPersisted(t *testing.T, ctx context.Context, srv config.MCPServer, want string) { //nolint:revive // t before ctx matches this package's other test-helper signatures
	t.Helper()

	path, err := TokenPath(srv.Name)
	if err != nil {
		t.Fatal(err)
	}

	tf, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}

	assertLoginPersisted(t, tf, srv, want)
	assertPersistedTokenWorks(t, ctx, srv)
}

func replay(t *testing.T, base, state string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/mcp/callback?code=again&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}
