package e2e

// A login against a daemon crosses three boundaries at once — the CLI starts it, a browser finishes it on a route with no credentials, and a poll later spends the token — so the first test walks all three rather than any one.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/cli"
)

// oauthFixture is an MCP server behind an authorization server that behaves like a real one: /authorize REDIRECTS the browser to the redirect_uri it was given, which is the half a loopback-only fake never had to get right.
type oauthFixture struct {
	server *httptest.Server
	// redirectURIs records every redirect_uri a client registered, so a test can assert the daemon asked for its PUBLIC address and not a loopback one.
	redirectURIs atomic.Value
	// authorized counts MCP requests that arrived carrying the issued token.
	authorized atomic.Int64
}

const oauthFixtureToken = "issued-to-the-daemon"

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()

	fixture := &oauthFixture{}
	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)

	base := fixture.server.URL

	tools := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "oauth-fixture", Version: "v0"}, nil)
	tools.AddTool(&sdkmcp.Tool{Name: "list_items", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `[{"id":"ITEM-1"}]`}}}, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return tools }, nil)

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+oauthFixtureToken {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		fixture.authorized.Add(1)
		handler.ServeHTTP(w, r)
	})

	writeJSON := func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(w).Encode(body)
		if err != nil {
			t.Errorf("oauth fixture: %v", err)
		}
	}

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
	})

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
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

	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any

		_ = json.NewDecoder(r.Body).Decode(&meta)

		fixture.redirectURIs.Store(meta["redirect_uris"])
		writeJSON(w, map[string]any{"client_id": "fixture-client", "redirect_uris": meta["redirect_uris"], "grant_types": meta["grant_types"]})
	})

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		http.Redirect(w, r, query.Get("redirect_uri")+"?code=fixture-code&state="+query.Get("state"), http.StatusFound) //nolint:gosec // redirecting to the caller's redirect_uri is what an authorization server IS; this one is a test fixture
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"access_token": oauthFixtureToken, "refresh_token": "fixture-refresh", "token_type": "Bearer", "expires_in": 3600})
	})

	return fixture
}

// fakeBrowser points $BROWSER at a script that follows redirects and sends NO credentials — which is the property under test: a provider's redirect arrives as a bare navigation, so the callback has to be reachable without the daemon's password.
func fakeBrowser(t *testing.T) {
	t.Helper()

	script := filepath.Join(t.TempDir(), "browser")

	err := os.WriteFile(script, []byte("#!/bin/sh\nexec curl -sL -o /dev/null \"$1\"\n"), 0o700) //nolint:gosec // a test stub that has to be executable
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("BROWSER", script)
}

// Not t.Parallel(): startWeb backgrounds cli.Run in this process, and the token directory and $BROWSER are process environment.
func TestMCPLoginAgainstADaemonAuthorizesItsPipelines(t *testing.T) {
	const (
		user = "ops"
		pass = "correct-horse-battery"
	)

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	fakeBrowser(t)

	fixture := newOAuthFixture(t)
	ran := filepath.Join(dir, "ran.log")
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true
mcp_servers:
- name: tracker
  endpoint: `+fixture.server.URL+`/mcp
  auth: { type: oauth }
resource_types:
- name: tracker-items
  config:
    mcp:
      server: tracker
      check:
        tool: list_items
resources:
- name: items
  type: tracker-items
  source: {}
jobs:
- name: react
  plan:
  - get: items
    trigger: true
  - task: record
    inputs: [items]
    run: cat items/version.json >> `+ran+`
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--interval", "100ms",
		"--basic-auth-username", user, "--basic-auth-password", pass)
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)
	target := authTarget(served.addr, user, pass)

	// An unauthorized server must not stop the pipeline being SET: the login resolves the server from the served configuration, so refusing the set would leave nothing to log in against.
	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", target})
	if err != nil {
		t.Fatalf("set a pipeline whose oauth server is not yet authorized: %v", err)
	}

	time.Sleep(400 * time.Millisecond)

	if fileExists(ran) || fixture.authorized.Load() != 0 {
		t.Fatal("the job ran before anybody logged in")
	}

	// Somebody starts a login and walks away. The one that follows must REPLACE it — a token file is keyed by server name, so two would race to write one — and the abandoned state must stop being worth anything.
	abandoned := startLoginOverHTTP(t, served.addr, name, user, pass)

	out := captureStdout(t, func() {
		err = cli.Run([]string{"mcp", "login", "tracker", "-p", name, "--target", target})
	})
	if err != nil {
		t.Fatalf("steps mcp login against the daemon: %v\n%s", err, out)
	}

	loginSaidWhatItShould(t, out, fixture, pass)
	theDaemonHoldsTheToken(t, fixture, served.addr, dir)

	if code, _ := authGet(t, served.addr, "/mcp/callback?code=late&state="+abandoned, "", ""); code != http.StatusNotFound {
		t.Errorf("the abandoned login's redirect = %d, want 404: it was replaced, and its state must have died with it", code)
	}

	// Left pending on purpose: the daemon's shutdown has to take a login waiting on a browser down with it, which goleak is watching for.
	startLoginOverHTTP(t, served.addr, name, user, pass)

	thePipelineSpendsTheToken(t, fixture, ran)
}

func loginSaidWhatItShould(t *testing.T, out string, fixture *oauthFixture, pass string) {
	t.Helper()

	if !strings.Contains(out, fixture.server.URL+"/authorize") {
		t.Errorf("the login did not print the authorization URL:\n%s", out)
	}

	if strings.Contains(out, pass) {
		t.Errorf("the login printed the daemon's password:\n%s", out)
	}
}

// The redirect has to name the DAEMON, at the address the CLI reached it on and with the credentials taken off: a loopback URI would send the browser to the operator's own laptop, and userinfo would hand the password to the provider.
func theDaemonHoldsTheToken(t *testing.T, fixture *oauthFixture, addr, dir string) {
	t.Helper()

	registered, _ := fixture.redirectURIs.Load().([]any)
	if len(registered) != 1 || registered[0] != "http://"+addr+"/mcp/callback" {
		t.Errorf("registered redirect_uris = %v, want exactly http://%s/mcp/callback", registered, addr)
	}

	// Found rather than named, since os.UserConfigDir puts it under $XDG_CONFIG_HOME on Linux and $HOME/Library on macOS.
	saved := 0

	_ = filepath.WalkDir(dir, func(found string, _ os.DirEntry, _ error) error {
		if filepath.Base(found) == "tracker.json" {
			saved++
		}

		return nil
	})

	if saved != 1 {
		t.Errorf("found %d tracker.json token files under the daemon's config dir, want 1", saved)
	}
}

func thePipelineSpendsTheToken(t *testing.T, fixture *oauthFixture, ran string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for !fileExists(ran) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	body, _ := os.ReadFile(ran) //nolint:gosec // a path this test made
	if !strings.Contains(string(body), "ITEM-1") {
		t.Fatalf("the pipeline never used the token the login obtained (ran.log = %q, authorized requests = %d)", body, fixture.authorized.Load())
	}
}

// startLoginOverHTTP starts a login the way the CLI does and returns the state of the authorization URL it was given, playing no browser: the login is left waiting.
func startLoginOverHTTP(t *testing.T, addr, pipeline, user, pass string) string {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	target := "http://" + addr + "/api/pipelines/" + pipeline + "/mcp/tracker/login"

	call := func(method, body string) map[string]string {
		req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(user, pass)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}

		defer func() { _ = resp.Body.Close() }()

		var status map[string]string

		_ = json.NewDecoder(resp.Body).Decode(&status)

		return status
	}

	call(http.MethodPost, `{"base":"http://`+addr+`"}`)

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if authURL := call(http.MethodGet, "")["authorize_url"]; authURL != "" {
			_, state, _ := strings.Cut(authURL, "state=")
			state, _, _ = strings.Cut(state, "&")

			return state
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("the daemon never produced an authorization URL")

	return ""
}
