package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/web"
)

func TestRedirectFor(t *testing.T) {
	t.Parallel()

	for base, want := range map[string]string{
		"https://steps.example.com":  "https://steps.example.com/mcp/callback",
		"https://steps.example.com/": "https://steps.example.com/mcp/callback",
		"http://127.0.0.1:8088":      "http://127.0.0.1:8088/mcp/callback",
		// A daemon behind a reverse proxy's path prefix keeps it.
		"https://host/steps": "https://host/steps/mcp/callback",
	} {
		got, err := redirectFor(base)
		if err != nil || got != want {
			t.Errorf("redirectFor(%q) = %q, %v; want %q", base, got, err, want)
		}
	}

	for _, base := range []string{
		"", "steps.example.com", "ftp://host", "https://", "https://host?x=1", "https://host#frag",
	} {
		got, err := redirectFor(base)
		if err == nil {
			t.Errorf("redirectFor(%q) = %q, want it refused", base, got)
		}
	}
}

// The redirect URI is sent to the authorization server and kept by it, so a password on it has left the building. Refused rather than stripped: the CLI strips, and a client that still sent one has a bug worth hearing about.
func TestRedirectForRefusesCredentials(t *testing.T) {
	t.Parallel()

	_, err := redirectFor("https://ops:hunter2@steps.example.com")
	if err == nil {
		t.Fatal("a base carrying credentials was turned into a redirect URI")
	}

	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the refusal repeats the password: %v", err)
	}
}

const loginPipeline = `
mcp_servers:
- name: tracker
  endpoint: http://127.0.0.1:1/mcp
  auth: { type: oauth }
- name: keyed
  endpoint: http://127.0.0.1:1/other
  # $PATH because the NAME is all this fixture needs and a set is refused for a bearer server whose variable is unset — see config.checkMCPServers.
  auth: { type: bearer, api_key_env: PATH }
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`

// The server comes from the configuration being SERVED, and what cannot be logged in to is refused in a sentence before anything is started.
func TestStartLoginRefusesWhatItCannotAuthorize(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	setPipeline(t, held, "app", loginPipeline)

	target := held.server.Lookup("app")
	base := web.LoginRequest{Base: "https://steps.example.com"}

	_, err := held.StartLogin(target, "nosuch", base)
	if err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("a server the pipeline does not declare = %v, want it named", err)
	}

	_, err = held.StartLogin(target, "keyed", base)
	if err == nil || !strings.Contains(err.Error(), "nothing to log in to") {
		t.Errorf("a bearer server = %v, want it refused as having nothing to log in to", err)
	}

	_, err = held.StartLogin(target, "tracker", web.LoginRequest{Base: "not a url"})
	if err == nil {
		t.Error("a login with no usable base was started")
	}

	if _, found := held.LoginStatus("tracker"); found {
		t.Error("a refused start left a login behind")
	}
}

// A login that cannot reach its server fails with the flow's own words, and the daemon's Close does not wait ten minutes for a browser that is never coming.
func TestAFailedLoginReportsWhyAndLetsGo(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	setPipeline(t, held, "app", loginPipeline)

	status, err := held.StartLogin(held.server.Lookup("app"), "tracker", web.LoginRequest{Base: "https://steps.example.com"})
	if err != nil || status.State != web.LoginPending {
		t.Fatalf("start = %+v, %v; want pending", status, err)
	}

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		status, _ = held.LoginStatus("tracker")
		if status.State != web.LoginPending {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	if status.State != web.LoginFailed || !strings.Contains(status.Message, "tracker") {
		t.Errorf("a login against an unreachable server = %+v, want failed, naming the server", status)
	}

	if held.LoginCallback("anything") != nil || held.LoginCallback("") != nil {
		t.Error("a settled login still answers for a state")
	}
}

func TestMCPLoginNamesExactlyOnePipeline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "p.yml")

	err := os.WriteFile(path, []byte(loginPipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"mcp", "login", "tracker"},
		{"mcp", "login", "tracker", "-c", path, "-p", "app"},
	} {
		err := Run(args)
		if err == nil || !strings.Contains(err.Error(), "-c <pipeline.yml>") || !strings.Contains(err.Error(), "-p <name>") {
			t.Errorf("%v = %v, want both forms named", args, err)
		}
	}
}

// $BROWSER is how an operator on macOS chooses a browser at all, and how the end-to-end suite plays one.
func TestOpenBrowserHonoursBROWSER(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	script := filepath.Join(dir, "browser")

	err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$1\" > "+seen+"\n"), 0o700) //nolint:gosec // a test stub that has to be executable
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("BROWSER", script)

	err = openBrowser("https://as.example/authorize?state=x")
	if err != nil {
		t.Fatalf("openBrowser: %v", err)
	}

	got, _ := os.ReadFile(seen) //nolint:gosec // a path this test made
	if string(got) != "https://as.example/authorize?state=x" {
		t.Errorf("$BROWSER was handed %q", got)
	}
}

// waitingAuthServer answers discovery and registration, so a login gets as far as WAITING for a browser — the only state in which what happens to an unfinished login can be observed at all.
func waitingAuthServer(t *testing.T) string {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	reply := func(body map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}
	}

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", reply(map[string]any{
		"resource": server.URL + "/mcp", "authorization_servers": []string{server.URL},
	}))
	mux.HandleFunc("/.well-known/oauth-authorization-server", reply(map[string]any{
		"issuer":                           server.URL,
		"authorization_endpoint":           server.URL + "/authorize",
		"token_endpoint":                   server.URL + "/token",
		"registration_endpoint":            server.URL + "/register",
		"response_types_supported":         []string{"code"},
		"code_challenge_methods_supported": []string{"S256"},
	}))
	mux.HandleFunc("/register", reply(map[string]any{"client_id": "waiting-client"}))

	return server.URL
}

// startWaitingLogin starts a login and returns it once it is waiting on a browser nobody will bring.
func startWaitingLogin(t *testing.T, held *daemon) *pendingLogin {
	t.Helper()

	_, err := held.StartLogin(held.server.Lookup("app"), "tracker", web.LoginRequest{Base: "https://steps.example.com"})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	held.logins.mu.Lock()
	pending := held.held["tracker"]
	held.logins.mu.Unlock()

	deadline := time.Now().Add(20 * time.Second)

	for pending.read().AuthorizeURL == "" {
		if time.Now().After(deadline) {
			t.Fatalf("the login never reached its browser step: %+v", pending.read())
		}

		time.Sleep(10 * time.Millisecond)
	}

	return pending
}

func waitingPipeline(authServer string) string {
	return strings.Replace(loginPipeline, "http://127.0.0.1:1/mcp", authServer+"/mcp", 1)
}

// Replaced in the registry is not the same as stopped: without the cancel, every re-login leaves the one before it holding a goroutine for ten minutes.
func TestASecondLoginStopsTheFirst(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	setPipeline(t, held, "app", waitingPipeline(waitingAuthServer(t)))

	first := startWaitingLogin(t, held)
	second := startWaitingLogin(t, held)

	deadline := time.Now().Add(10 * time.Second)

	for first.read().State == web.LoginPending && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if first.read().State != web.LoginFailed {
		t.Errorf("the replaced login is still %q: it was dropped from the registry and left running", first.read().State)
	}

	if second.read().State != web.LoginPending {
		t.Errorf("the login that replaced it is %q, want it still waiting", second.read().State)
	}
}

// Close returns with every login's goroutine GONE, not merely told to go — on a context that outlives the daemon, which is what makes the difference visible: the process's own exit would otherwise cancel them a moment later and hide it.
func TestCloseTakesAWaitingLoginDownWithIt(t *testing.T) {
	t.Parallel()

	local := web.NewLocalRunner(nil, nil, 1, false)

	server, err := web.New(nil, local)
	if err != nil {
		t.Fatal(err)
	}

	held := newDaemon(context.Background(), server, local, filepath.Join(t.TempDir(), "steps.db"), ExecFlags{}, HistoryFlags{}, time.Hour)
	setPipeline(t, held, "app", waitingPipeline(waitingAuthServer(t)))

	pending := startWaitingLogin(t, held)

	held.Close()

	if state := pending.read().State; state != web.LoginFailed {
		t.Errorf("after Close the login is %q, want it ended: Close returned while a login was still running", state)
	}
}
