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

	// Somebody starts a login and walks away. The one that follows must REPLACE it — a token file is keyed by pipeline and server name, so two would race to write one — and the abandoned state must stop being worth anything.
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

	// Waits on the content, not the file: the job's shell creates ran.log before it writes to it, and a read in between saw an empty file under load.
	var body []byte

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body, _ = os.ReadFile(ran) //nolint:gosec // a path this test made
		if strings.Contains(string(body), "ITEM-1") {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("the pipeline never used the token the login obtained (ran.log = %q, authorized requests = %d)", body, fixture.authorized.Load())
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

// trackerPipeline is one oauth server feeding a job that appends what it read to ran, so each pipeline's runs are visible on their own.
func trackerPipeline(endpoint, ran string) string {
	return `
defaults:
  preflight:
    disabled: true
mcp_servers:
- name: tracker
  endpoint: ` + endpoint + `/mcp
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
    run: cat items/version.json >> ` + ran + `
`
}

// mcpLogins is where this process's logins live, per os.UserConfigDir under the environment the test set.
func mcpLogins(t *testing.T) string {
	t.Helper()

	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}

	return filepath.Join(root, "steps", "mcp")
}

// Two pipelines on one daemon declare the same server. A login for one must leave the other's page, status route and runs exactly as they were, and the login must follow its pipeline through a rename and go with it on a destroy.
//
// Not t.Parallel(): startWeb backgrounds cli.Run in this process, and the token directory and $BROWSER are process environment.
func TestAnMCPLoginBelongsToOnePipeline(t *testing.T) {
	const (
		user = "ops"
		pass = "correct-horse-battery"
	)

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	fakeBrowser(t)

	fixture := newOAuthFixture(t)
	logs := map[string]string{"app": filepath.Join(dir, "app.log"), "other": filepath.Join(dir, "other.log")}

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--interval", "100ms",
		"--basic-auth-username", user, "--basic-auth-password", pass)
	defer served.stopIfRunning(t)

	target := authTarget(served.addr, user, pass)

	for name, ran := range logs {
		path := filepath.Join(dir, name+".yml")
		writePipelineFile(t, path, trackerPipeline(fixture.server.URL, ran))

		err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", target})
		if err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	out := captureStdout(t, func() {
		err := cli.Run([]string{"mcp", "login", "tracker", "-p", "app", "--target", target})
		if err != nil {
			t.Errorf("mcp login for app: %v", err)
		}
	})
	if t.Failed() {
		t.Fatal(out)
	}

	thePipelineSpendsTheToken(t, fixture, logs["app"])
	onlyAppIsLoggedIn(t, served.addr, user, pass, logs["other"])

	browser := newBrowser(t, served.addr, user, pass)
	theTabEventuallySays(t, browser, "/p/app/mcp", "renews automatically")

	if body := browser.get(t, "/p/other/mcp"); !strings.Contains(body, "needs login") || !strings.Contains(body, "saved per pipeline") {
		t.Errorf("other's mcp tab after a login for app, want needs login and the note saying why:\n%s", body)
	}

	theLoginFollowsARename(t, fixture, browser, target)
	aRenameOntoLoginsIsRefusedAndADestroyRemovesOnlyItsOwn(t, served.addr, user, pass, target)
}

// A rename keeps history, so it keeps the login, and the pipeline goes on spending it.
func theLoginFollowsARename(t *testing.T, fixture *oauthFixture, browser *browserClient, target string) {
	t.Helper()

	err := cli.Run([]string{"pipeline", "rename", "-p", "app", "--to", "app2", "--target", target})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}

	if !fileExists(filepath.Join(mcpLogins(t), "app2", "tracker.json")) {
		t.Error("the login did not follow the rename")
	}

	theTabEventuallySays(t, browser, "/p/app2/mcp", "renews automatically")

	spent := fixture.authorized.Load()
	for deadline := time.Now().Add(10 * time.Second); fixture.authorized.Load() == spent; {
		if time.Now().After(deadline) {
			t.Fatal("the renamed pipeline never spent its login again")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// Renaming onto somebody's logins would inherit or destroy them; destroying a pipeline takes its own and nobody else's.
func aRenameOntoLoginsIsRefusedAndADestroyRemovesOnlyItsOwn(t *testing.T, addr, user, pass, target string) {
	t.Helper()

	seeded := filepath.Join(mcpLogins(t), "taken", "tracker.json")

	err := os.MkdirAll(filepath.Dir(seeded), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, seeded, "{}")

	err = cli.Run([]string{"pipeline", "rename", "-p", "app2", "--to", "taken", "--target", target})
	if err == nil || !strings.Contains(err.Error(), "mcp logins") {
		t.Errorf("rename onto existing logins = %v, want refused naming them", err)
	}

	if code, _ := authGet(t, addr, "/p/app2", user, pass); code != http.StatusOK {
		t.Errorf("/p/app2 = %d after a refused rename, want it still served", code)
	}

	err = cli.Run([]string{"pipeline", "destroy", "-p", "app2", "-n", "--target", target})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}

	_, err = os.Stat(filepath.Join(mcpLogins(t), "app2"))
	if !os.IsNotExist(err) {
		t.Errorf("app2's logins survived its destroy: %v", err)
	}

	if !fileExists(seeded) {
		t.Error("a destroy removed another name's logins")
	}
}

func onlyAppIsLoggedIn(t *testing.T, addr, user, pass, otherLog string) {
	t.Helper()

	time.Sleep(500 * time.Millisecond)

	if fileExists(otherLog) {
		t.Error("other ran on app's login")
	}

	if code, _ := authGet(t, addr, "/api/pipelines/other/mcp/tracker/login", user, pass); code != http.StatusNotFound {
		t.Errorf("other's login status = %d, want 404: the login was app's", code)
	}

	var saved []string

	_ = filepath.WalkDir(mcpLogins(t), func(found string, _ os.DirEntry, _ error) error {
		if filepath.Base(found) == "tracker.json" {
			saved = append(saved, filepath.Base(filepath.Dir(found)))
		}

		return nil
	})

	if len(saved) != 1 || saved[0] != "app" {
		t.Errorf("tracker.json saved under %v, want exactly [app]", saved)
	}
}

// A local login is filed under the name --name gives the file, the same one a run with that --name uses — and a run under any other name is not logged in, and says for which pipeline.
//
// Not t.Parallel(): the token directory and $BROWSER are process environment.
func TestALocalLoginFollowsDashName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	fakeBrowser(t)

	fixture := newOAuthFixture(t)
	path := filepath.Join(dir, "p.yml")
	writePipelineFile(t, path, strings.Replace(trackerPipeline(fixture.server.URL, filepath.Join(dir, "ran.log")),
		"defaults:\n  preflight:\n    disabled: true\n", "", 1))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"mcp", "login", "tracker", "-c", path, "--name", "infra=" + path})
		if err != nil {
			t.Errorf("local mcp login: %v", err)
		}
	})

	if !strings.Contains(out, `for pipeline "infra"`) {
		t.Errorf("the login did not say which pipeline it saved for:\n%s", out)
	}

	if !fileExists(filepath.Join(mcpLogins(t), "infra", "tracker.json")) {
		t.Fatal("the login was not saved under --name's pipeline")
	}

	captureStdout(t, func() {
		err := cli.Run([]string{"validate", "--live", path, "--name", "infra=" + path})
		if err != nil {
			t.Errorf("validate --live under the logged-in name: %v", err)
		}
	})

	captureStdout(t, func() {
		err := cli.Run([]string{"test", path, "--db", filepath.Join(dir, "p.db")})
		if err == nil || !strings.Contains(err.Error(), `not authorized for pipeline "p"`) {
			t.Errorf("a run under the file's own name = %v, want it not authorized for pipeline p", err)
		}
	})
}
