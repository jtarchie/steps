package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// Distinctive on purpose: a one-letter token appears inside every sentence a status cell could hold, so a leak check against one asserts nothing.
const secretToken = "tok-must-never-be-rendered"

const mcpStatePipeline = `
mcp_servers:
- name: tracker
  endpoint: https://tracker.example/mcp
  auth: { type: oauth }
- name: perstep
  command: sh
  cwd: repo
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`

// A saved token is a file this daemon owns, and every state it can be in has to read as itself on a page: a login that was never done, one done against a different endpoint, and one whose access token died with nothing to renew it.
//
// Not t.Parallel(): the token directory is process environment.
func TestMCPStateReportsWhatASavedTokenIsWorth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)

	held := servingDaemon(t)
	setPipeline(t, held, "app", mcpStatePipeline)

	target := held.server.Lookup("app")
	live := "https://tracker.example/mcp"

	for _, test := range []struct {
		name      string
		token     *stepsmcp.TokenFile
		connected bool
		detail    string
	}{
		{
			name:   "nobody has logged in",
			detail: "needs login",
		},
		{
			// Keyed by NAME alone, so another pipeline's `tracker` can leave one here for a different endpoint — which a run refuses and a page must not call connected.
			name:   "a token issued for another endpoint",
			token:  &stepsmcp.TokenFile{Endpoint: "https://somewhere.else/mcp", AccessToken: secretToken, RefreshToken: "r"},
			detail: "different endpoint",
		},
		{
			name:   "an access token that expired with nothing to renew it",
			token:  &stepsmcp.TokenFile{Endpoint: live, AccessToken: secretToken, Expiry: time.Now().Add(-time.Hour)},
			detail: "expired",
		},
		{
			name:      "a token that can renew itself",
			token:     &stepsmcp.TokenFile{Endpoint: live, AccessToken: secretToken, RefreshToken: "r"},
			connected: true,
			detail:    "renews automatically",
		},
		{
			// It works right now, which is all a run needs — but it is the one state nobody gets a second warning about.
			name:      "a token that works and cannot be renewed",
			token:     &stepsmcp.TokenFile{Endpoint: live, AccessToken: secretToken, Expiry: time.Now().Add(time.Hour)},
			connected: true,
			detail:    "cannot renew itself",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeTokenFile(t, "tracker", test.token)

			got := held.MCPState(target, "tracker").Credential
			if got.Connected != test.connected || !strings.Contains(got.Detail, test.detail) {
				t.Errorf("%+v, want connected=%v saying %q", got, test.connected, test.detail)
			}

			// The page renders this, and the token is the one thing that must never reach one.
			if test.token != nil && strings.Contains(got.Detail, test.token.AccessToken) {
				t.Errorf("the credential state carries the credential: %q", got.Detail)
			}
		})
	}
}

// A relative cwd: resolves against an agent step's workspace, which exists only during a run — so probing from the daemon's own directory tests a server nobody configured.
func TestStartProbeRefusesWhatItCannotHonestlyProbe(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	setPipeline(t, held, "app", mcpStatePipeline)

	target := held.server.Lookup("app")

	err := held.StartProbe(target, "perstep")
	if err == nil || !strings.Contains(err.Error(), "resolves per step") {
		t.Errorf("probing a per-step server = %v, want it refused with the reason", err)
	}

	err = held.StartProbe(target, "nosuch")
	if err == nil {
		t.Error("probing a server the pipeline does not declare was allowed")
	}
}

// The result is remembered against the configuration it describes, so a `steps pipeline set` that moves an endpoint does not leave a green cell describing a server this pipeline no longer has.
func TestAProbeResultDoesNotSurviveTheConfigurationItDescribes(t *testing.T) {
	t.Parallel()

	// A real MCP endpoint, because a probe's whole job is to find out what one actually answers.
	tools := mcpCLIFixtureServer(t)

	held := servingDaemon(t)
	setPipeline(t, held, "app", probePipeline(tools.URL))

	target := held.server.Lookup("app")

	err := held.StartProbe(target, "tracker")
	if err != nil {
		t.Fatalf("StartProbe: %v", err)
	}

	probe := waitForProbe(t, held, target, "tracker")
	if !probe.OK || !strings.Contains(probe.Detail, "1 tool") {
		t.Fatalf("probe of a live server = %+v, want the tools it exposes", probe)
	}

	// The same server name, a different endpoint: the old answer describes something else now.
	setPipeline(t, held, "app", probePipeline(tools.URL+"/moved"))

	if got := held.MCPState(held.server.Lookup("app"), "tracker").Probe; got != nil {
		t.Errorf("a probe result outlived the endpoint it described: %+v", got)
	}
}

// A probe that fails is the answer, not an error: the tab's whole job is to say what a server does when something actually connects to it, and "nothing is listening" is exactly that.
func TestAFailedProbeIsTheAnswerTheRowCarries(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)

	// A port nothing is listening on, and a timeout short enough that this test is not thirty seconds long.
	setPipeline(t, held, "app", `
defaults:
  preflight:
    timeout: 2s
mcp_servers:
- name: tracker
  endpoint: http://127.0.0.1:1/mcp
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`)

	target := held.server.Lookup("app")

	err := held.StartProbe(target, "tracker")
	if err != nil {
		t.Fatalf("StartProbe: %v", err)
	}

	probe := waitForProbe(t, held, target, "tracker")
	if probe.OK || probe.Detail == "" {
		t.Errorf("probe of a dead endpoint = %+v, want the reason it did not answer", probe)
	}

	// The row already names the server, so the error's copies of the name are not what the cell's width is for.
	if strings.HasPrefix(probe.Detail, "mcp:") {
		t.Errorf("the probe detail keeps the noise the row already carries: %q", probe.Detail)
	}
}

// A server the pipeline does not declare has no state to report, and the page must not render a blank row as a working one.
func TestMCPStateOfAnUndeclaredServerSaysSo(t *testing.T) {
	t.Parallel()

	held := servingDaemon(t)
	setPipeline(t, held, "app", mcpStatePipeline)

	got := held.MCPState(held.server.Lookup("app"), "nosuch")
	if got.Credential.Connected || !strings.Contains(got.Credential.Detail, "nosuch") {
		t.Errorf("MCPState of an undeclared server = %+v, want it named as unknown", got)
	}
}

func probePipeline(endpoint string) string {
	return `
mcp_servers:
- name: tracker
  endpoint: ` + endpoint + `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`
}

func waitForProbe(t *testing.T, held *daemon, target *web.Pipeline, server string) web.MCPProbe {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		probe := held.MCPState(target, server).Probe
		if probe != nil && !probe.Running {
			return *probe
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("the probe never finished")

	return web.MCPProbe{}
}

// writeTokenFile puts one saved credential where a login would have left it, or removes it for nil.
func writeTokenFile(t *testing.T, server string, token *stepsmcp.TokenFile) {
	t.Helper()

	path, err := stepsmcp.TokenPath(server)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	if token == nil {
		err = os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}

		return
	}

	body, err := json.Marshal(token) //nolint:gosec // G117: writing the token file IS this fixture's job; the struct is the format under test and every value in it is invented here
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, body, 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// A login's Return travels from a request into a Location header, so anything that could name another host would make this daemon an open redirector — somebody else's login ending on somebody else's page.
func TestALoginOnlyReturnsToThisDaemon(t *testing.T) {
	t.Parallel()

	for _, refused := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"http://127.0.0.1/p/app/mcp",
		"mcp",
	} {
		got, err := returnTo(refused)
		if err == nil {
			t.Errorf("returnTo(%q) = %q, want it refused", refused, got)
		}
	}

	kept, err := returnTo("/p/app/mcp")
	if err != nil || kept != "/p/app/mcp" {
		t.Errorf("returnTo(a path on this daemon) = %q, %v", kept, err)
	}

	// A terminal login has no page to go back to, and keeps the callback's plain sentence.
	none, err := returnTo("")
	if err != nil || none != "" {
		t.Errorf("returnTo(none) = %q, %v", none, err)
	}
}

// Two people click Connect on one server. The token file is keyed by name, so the second login REPLACES the first — and the first person is at a consent screen whose redirect now matches nothing. A 404 for a thing that did happen is the wrong answer when there is a page to say it on.
func TestAReplacedBrowserLoginIsSentBackToItsPage(t *testing.T) {
	t.Parallel()

	held := newLogins(t.Context())
	t.Cleanup(held.stop)

	// One more than the cap keeps, so the oldest has to have been let go of.
	states := make([]string, 0, staleLogins+2)
	for range cap(states) {
		states = append(states, pendingFor(t, held, "/p/app/mcp"))
	}

	replaced := states[:len(states)-1]

	handler := held.LoginCallback(replaced[len(replaced)-1])
	if handler == nil {
		t.Fatal("a replaced login's redirect was not answered at all")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp/callback?state="+replaced[len(replaced)-1], nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/p/app/mcp" {
		t.Errorf("a replaced login's redirect = %d %q, want 303 back to the tab", rec.Code, rec.Header().Get("Location"))
	}

	// Bounded, or a daemon nobody restarts accumulates one of these per click forever.
	if held.LoginCallback(replaced[0]) != nil {
		t.Errorf("all %d replaced logins are still remembered, which is a leak per click", len(replaced))
	}

	// Nothing answers a state that was never minted, which is what keeps the route from confirming it is listening for something.
	if held.LoginCallback("never-minted") != nil {
		t.Error("an unknown state was answered")
	}
}

// A login a TERMINAL started keeps the 404: the CLI polling for it is what reports the outcome there, and a state that still answered would say a replaced login is live.
func TestAReplacedTerminalLoginStaysA404(t *testing.T) {
	t.Parallel()

	held := newLogins(t.Context())
	t.Cleanup(held.stop)

	first := pendingFor(t, held, "")
	pendingFor(t, held, "")

	if handler := held.LoginCallback(first); handler != nil {
		t.Error("a replaced terminal login still answered its redirect")
	}
}

// A login replaced before discovery finished has no browser anywhere: no authorization URL was ever handed out, so there is nobody at a consent screen and nothing to remember.
func TestALoginReplacedBeforeItHadAURLIsNotRemembered(t *testing.T) {
	t.Parallel()

	held := newLogins(t.Context())
	t.Cleanup(held.stop)

	pending := &pendingLogin{cancel: func() {}, status: web.LoginStatus{State: web.LoginPending}}
	pending.hosted = stepsmcp.NewHostedCallback("http://daemon.test/mcp/callback", "/p/app/mcp", func(string) {})

	held.mu.Lock()
	held.keepStale(pending)
	remembered := len(held.stale)
	held.mu.Unlock()

	if remembered != 0 {
		t.Errorf("a login with no authorization URL was remembered %d times", remembered)
	}
}

// pendingFor puts one pending login in the map by hand, since starting a real one needs an authorization server to discover.
func pendingFor(t *testing.T, held *logins, back string) string {
	t.Helper()

	state := "state-" + back + "-" + time.Now().Format(time.RFC3339Nano)
	pending := &pendingLogin{cancel: func() {}, status: web.LoginStatus{
		State: web.LoginPending,
		// What the flow announces once discovery and registration are done, and the only place the state of its redirect is written down.
		AuthorizeURL: "https://as.example/authorize?state=" + url.QueryEscape(state),
	}}
	pending.hosted = stepsmcp.NewHostedCallback("http://daemon.test/mcp/callback", back, func(string) {})

	held.mu.Lock()
	if previous := held.held["tracker"]; previous != nil {
		previous.cancel()
		held.keepStale(previous)
	}

	held.held["tracker"] = pending
	held.mu.Unlock()

	return state
}
