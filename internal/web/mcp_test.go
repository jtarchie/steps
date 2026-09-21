package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// mcpAuthorizer is a token-holder the page can ask, with each answer settable so a test can put a row in a state a real login would take a provider to reach.
type mcpAuthorizer struct {
	fakeManager

	authorizeURL string
	credential   MCPCredential
	probe        *MCPProbe
	login        *LoginStatus

	started  []LoginRequest
	probed   []string
	startErr error
	probeErr error
	// startedID and statusID differ when a login has been replaced under its server name, which is what a second Connect does.
	startedID string
	statusID  string
}

func (m *mcpAuthorizer) StartLogin(_ *Pipeline, _ string, req LoginRequest) (LoginStatus, error) {
	if m.startErr != nil {
		return LoginStatus{}, m.startErr
	}

	m.started = append(m.started, req)

	return LoginStatus{State: LoginPending, ID: m.startedID}, nil
}

func (m *mcpAuthorizer) LoginStatus(string) (LoginStatus, bool) {
	if m.login != nil {
		return *m.login, true
	}

	if m.authorizeURL == "" {
		return LoginStatus{}, false
	}

	return LoginStatus{State: LoginPending, AuthorizeURL: m.authorizeURL, ID: m.statusID}, true
}

func (m *mcpAuthorizer) LoginCallback(string) http.Handler { return nil }

func (m *mcpAuthorizer) MCPState(*Pipeline, string) MCPState {
	return MCPState{Credential: m.credential, Probe: m.probe}
}

func (m *mcpAuthorizer) StartProbe(_ *Pipeline, server string) error {
	if m.probeErr != nil {
		return m.probeErr
	}

	m.probed = append(m.probed, server)

	return nil
}

// mcpServerPipeline is a pipeline that declares one of each shape a status cell has to have a word for, since the column is the tab's whole reason to exist.
func mcpServerPipeline(t *testing.T, runner Runner) (*Server, *Pipeline, *mcpAuthorizer) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
mcp_servers:
- name: linear
  endpoint: https://mcp.linear.app/mcp
  auth: { type: oauth }
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: STEPS_TEST_UNSET_PAT }
- name: gopls
  command: steps-no-such-mcp-server
- name: perstep
  command: sh
  cwd: repo

agents:
- name: triager
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: linear
    tool: list_issues

jobs:
- name: build
  plan:
  - task: compile
    run: "true"
`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	pipeline := NewPipeline("demo", path, cfg, st, events.New(nil))

	server, err := New([]*Pipeline{pipeline}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	authorizer := &mcpAuthorizer{credential: MCPCredential{Detail: "needs login"}}
	server.SetManager(authorizer)

	return server, pipeline, authorizer
}

// The column the tab exists for: an unset variable, a command that is not installed, and a login nobody has done — each answered with no request made to anything.
func TestMCPTabSaysWhyEachServerIsNotWiredUp(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})

	code, body := get(t, server, "/p/demo/mcp")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/mcp = %d", code)
	}

	for _, want := range []string{
		// What each server IS, in the same words `steps mcp list` prints.
		"linear", "https://mcp.linear.app/mcp", "oauth",
		"github", "bearer $STEPS_TEST_UNSET_PAT",
		"gopls", "stdio",
		// Who breaks while it is disconnected.
		"agent triager",
		// Why each one is not usable, without a job having to fail first.
		"needs login",
		"$STEPS_TEST_UNSET_PAT is not set",
		// The quotes html/template escapes are not the point; the sentence is.
		"steps-no-such-mcp-server", "not found on PATH",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the mcp tab never says %q:\n%s", want, body)
		}
	}
}

// A readiness NOTHING on this machine answered is not a pass. The stylesheet draws the glyph from the st-<mark> class alone, so asserting the status text sits immediately after that class pins both halves: the right mark, and no second glyph of the row's own beside the one the CSS already inserts.
func TestAStatusNobodyAnsweredIsNotDrawnAsAPass(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})

	_, body := get(t, server, "/p/demo/mcp")
	if want := `st-skipped">not probed (cwd: repo resolves per step)`; !strings.Contains(body, want) {
		t.Errorf("a cwd: that resolves per step is not drawn as unanswered (%s):\n%s", want, body)
	}

	// The same question with the token-holder gone: nothing here could run a login, which is not the server working.
	server.SetManager(&fakeManager{})

	_, body = get(t, server, "/p/demo/mcp")
	if want := `st-skipped">this daemon cannot run an mcp login`; !strings.Contains(body, want) {
		t.Errorf("a daemon that cannot log in drew the row as a pass (want %s):\n%s", want, body)
	}
}

// A pipeline with no mcp_servers: pays no nav space for a feature it does not use, and the page it would link to is not there either.
func TestTheMCPTabAppearsOnlyForAPipelineThatDeclaresServers(t *testing.T) {
	t.Parallel()

	plain, _ := testPipeline(t)

	_, body := get(t, plain, "/p/demo")
	if strings.Contains(body, `/p/demo/mcp"`) {
		t.Errorf("a pipeline with no mcp_servers: got an mcp tab:\n%s", body)
	}

	if code, _ := get(t, plain, "/p/demo/mcp"); code != http.StatusNotFound {
		t.Errorf("GET /p/demo/mcp on a pipeline with no servers = %d, want 404", code)
	}

	withServers, _, _ := mcpServerPipeline(t, stubRunner{})

	_, body = get(t, withServers, "/p/demo")
	if !strings.Contains(body, `/p/demo/mcp"`) {
		t.Errorf("a pipeline that declares mcp_servers: has no mcp tab:\n%s", body)
	}
}

// --read-only is a statement about what a browser may make this daemon DO, and Connect makes it authorize and Test makes it dial — but "needs login" is exactly the diagnostic a read-only box should still show.
func TestReadOnlyKeepsTheDiagnosticAndWithholdsTheButtons(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, nil)

	code, body := get(t, server, "/p/demo/mcp")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/mcp = %d", code)
	}

	if !strings.Contains(body, "needs login") {
		t.Errorf("a read-only daemon hid the diagnostic:\n%s", body)
	}

	for _, gone := range []string{"/mcp/linear/connect", "/mcp/linear/test"} {
		if strings.Contains(body, gone) {
			t.Errorf("a read-only daemon offered %s:\n%s", gone, body)
		}
	}

	// The CLI is what is left, so the page has to name it rather than leaving a reader with no way through.
	if !strings.Contains(body, "steps mcp login") {
		t.Errorf("a read-only tab does not say how to authorize from a terminal:\n%s", body)
	}

	for _, target := range []string{"/p/demo/mcp/linear/connect", "/p/demo/mcp/linear/test"} {
		if code := post(t, server, target, nil); code != http.StatusForbidden {
			t.Errorf("POST %s on a read-only daemon = %d, want 403", target, code)
		}
	}

	if len(authorizer.started)+len(authorizer.probed) != 0 {
		t.Error("a read-only daemon started a login or a probe anyway")
	}
}

// One POST is the whole flow, so the response has to BE the trip to the provider: the CLI's two extra halves (print the URL, poll for the result) exist only because a terminal is not a browser.
func TestConnectSendsTheBrowserToTheProviderAndAsksToComeBack(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})
	authorizer.authorizeURL = "https://as.example/authorize?state=minted"

	code, location := postOrigin(t, server, "/p/demo/mcp/linear/connect", "http://example.test")
	if code != http.StatusSeeOther {
		t.Fatalf("POST connect = %d, want 303", code)
	}

	if location != authorizer.authorizeURL {
		t.Errorf("connect sent the browser to %q, want the provider's authorization URL", location)
	}

	if len(authorizer.started) != 1 {
		t.Fatalf("connect started %d logins, want 1", len(authorizer.started))
	}

	// The redirect URI is built from this, and the request alone cannot answer it: behind a proxy the scheme is the proxy's.
	if got := authorizer.started[0].Base; got != "http://example.test" {
		t.Errorf("the login was started against %q, want the browser's own origin", got)
	}

	// Without it, a token exchange that fails after the callback has answered has nowhere to be reported.
	if got := authorizer.started[0].Return; got != "/p/demo/mcp" {
		t.Errorf("the login returns to %q, want the tab it was started from", got)
	}
}

// A POST with no Origin is not a browser, and the address a redirect URI is built from is the one thing it cannot supply — so it is refused with the command that does work rather than a login registered against a guess.
func TestConnectWithoutAnOriginIsRefusedAndNamesTheCLI(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})

	code := post(t, server, "/p/demo/mcp/linear/connect", nil)
	if code != http.StatusBadRequest {
		t.Errorf("POST connect with no Origin = %d, want 400", code)
	}

	if len(authorizer.started) != 0 {
		t.Error("a login was started without an address to send the provider back to")
	}
}

// Discovery and registration are two round trips, so the URL is not there when the click arrives. Waiting is what makes the button one action; giving up ON THE TAB is what keeps a provider that is down from being an error page.
func TestConnectFallsBackToTheTabWhenNoURLArrives(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})
	authorizer.login = &LoginStatus{State: LoginFailed, Message: "discovery failed"}

	code, location := postOrigin(t, server, "/p/demo/mcp/linear/connect", "http://example.test")
	if code != http.StatusSeeOther || location != "/p/demo/mcp" {
		t.Errorf("a login that failed before producing a URL = %d %q, want 303 back to the tab", code, location)
	}
}

// The failure a page must show, because it happens after the browser has already been sent back here: the callback answered, and the exchange 400d.
func TestTheTabReportsALoginThatFailedAfterTheRedirect(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})
	authorizer.login = &LoginStatus{State: LoginFailed, Message: "token exchange refused the code"}

	_, body := get(t, server, "/p/demo/mcp")
	if !strings.Contains(body, "token exchange refused the code") {
		t.Errorf("the tab does not report a login that failed after the redirect:\n%s", body)
	}
}

// Test is the live half, and it is detached: the click comes back at once and the row says what it is doing, because a preflight timeout is thirty seconds and this page is never otherwise slow.
func TestTheTabShowsAProbeWhileItRunsAndAfterItLands(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})

	code, location := postOrigin(t, server, "/p/demo/mcp/linear/test", "http://example.test")
	if code != http.StatusSeeOther || location != "/p/demo/mcp" {
		t.Fatalf("POST test = %d %q, want 303 back to the tab", code, location)
	}

	if len(authorizer.probed) != 1 || authorizer.probed[0] != "linear" {
		t.Fatalf("test probed %v, want linear once", authorizer.probed)
	}

	authorizer.probe = &MCPProbe{Running: true, At: time.Now()}

	_, body := get(t, server, "/p/demo/mcp")
	if !strings.Contains(body, "testing…") {
		t.Errorf("a probe in flight is not shown as one:\n%s", body)
	}

	authorizer.probe = &MCPProbe{OK: true, Detail: "7 tools", At: time.Now()}

	_, body = get(t, server, "/p/demo/mcp")
	if !strings.Contains(body, "7 tools") {
		t.Errorf("a finished probe is not reported:\n%s", body)
	}
}

// A manager that is not an Authorizer cannot answer a token question, and must not let the page imply it can: a Connect button that 501s is worse than a sentence saying this build has nowhere to put a token.
func TestADaemonWithNoAuthorizerSaysSoRatherThanOfferingALogin(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, stubRunner{})
	server.SetManager(&fakeManager{})

	code, body := get(t, server, "/p/demo/mcp")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/mcp with no authorizer = %d", code)
	}

	if !strings.Contains(body, "cannot run an mcp login") {
		t.Errorf("the tab does not say this daemon cannot log in:\n%s", body)
	}

	// The rest of the page is configuration, which needs no token-holder at all.
	if !strings.Contains(body, "not found on PATH") {
		t.Errorf("the static column went missing with the authorizer:\n%s", body)
	}

	if code := postOriginCode(t, server, "/p/demo/mcp/linear/connect", "http://example.test"); code != http.StatusNotImplemented {
		t.Errorf("connect against a daemon that cannot log in = %d, want 501", code)
	}
}

// A login is tracked by server NAME, so a second Connect on that name takes it over — and two pipelines may declare one name against DIFFERENT endpoints. The reader whose login was replaced must not be handed the replacement's consent screen: they would be authorizing something they never clicked on.
func TestConnectWillNotFollowALoginThatReplacedItsOwn(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})

	// What StartLogin handed back, against what the server name answers by the time the URL exists.
	authorizer.startedID = "the-click-that-got-here-first"
	authorizer.statusID = "a-later-click-on-the-same-name"
	authorizer.authorizeURL = "https://as.example/authorize?state=somebody-elses"

	code, location := postOrigin(t, server, "/p/demo/mcp/linear/connect", "http://example.test")
	if code != http.StatusSeeOther {
		t.Fatalf("POST connect = %d, want 303", code)
	}

	if location != "/p/demo/mcp" {
		t.Errorf("connect followed the login that replaced its own, to %q — the reader is at a consent screen they never asked for", location)
	}
}

// Nothing to log in TO is not a page error: a bearer or stdio server has no consent screen, and the refusal is the flow's own sentence rather than a button that quietly does nothing.
func TestConnectAndTestReportWhatTheTokenHolderRefused(t *testing.T) {
	t.Parallel()

	server, _, authorizer := mcpServerPipeline(t, stubRunner{})
	authorizer.startErr = errors.New(`mcp server "github" is not auth: {type: oauth}; nothing to log in to`)
	authorizer.probeErr = errors.New(`mcp server "perstep" cannot be probed from here: cwd: repo resolves per step`)

	code, _ := postOrigin(t, server, "/p/demo/mcp/github/connect", "http://example.test")
	if code != http.StatusBadRequest {
		t.Errorf("connect to a server with no login = %d, want 400", code)
	}

	code, _ = postOrigin(t, server, "/p/demo/mcp/github/test", "http://example.test")
	if code != http.StatusBadRequest {
		t.Errorf("testing a server that cannot be probed = %d, want 400", code)
	}
}

// postOrigin submits a form the way a browser does — with the Origin header sameOriginMutations checks and the handler reads — and returns where it was sent.
func postOrigin(t *testing.T, server *Server, target, origin string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, nil)
	req.Header.Set("Origin", origin)
	req.Host = strings.TrimPrefix(origin, "http://")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Header().Get("Location")
}

func postOriginCode(t *testing.T, server *Server, target, origin string) int {
	t.Helper()

	code, _ := postOrigin(t, server, target, origin)

	return code
}
