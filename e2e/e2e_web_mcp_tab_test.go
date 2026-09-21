package e2e

// The mcp tab and its Connect button. The seam is the whole point: a POST that resolves the server, a 303 out to the provider, the provider's redirect back into the daemon's callback, and the tab that then reads the token the login saved — no CLI, no URL carried between windows, no polling by anything but the page itself.

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// Not t.Parallel(): startWeb backgrounds cli.Run in this process, and the token directory is process environment.
func TestMCPTabConnectsAnOAuthServerFromTheBrowser(t *testing.T) {
	const (
		user = "ops"
		pass = "correct-horse-battery"
	)

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	t.Setenv("TRACKER_PAT", "a-static-token")

	fixture := newOAuthFixture(t)
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
mcp_servers:
- name: tracker
  endpoint: `+fixture.server.URL+`/mcp
  auth: { type: oauth }
- name: keyed
  endpoint: `+fixture.server.URL+`/mcp
  auth: { type: bearer, api_key_env: TRACKER_PAT }
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
  - task: work
    run: "true"
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"),
		"--basic-auth-username", user, "--basic-auth-password", pass)
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, user, pass)})
	if err != nil {
		t.Fatalf("set a pipeline whose oauth server is not yet authorized: %v", err)
	}

	tab := "/p/" + name + "/mcp"
	browser := newBrowser(t, served.addr, user, pass)

	theTabNamesWhatIsWiredUp(t, browser.get(t, tab))

	// One POST is the whole flow: the daemon resolves the server, waits out discovery, and sends the browser to the provider, which sends it back to the callback, which sends it back here.
	landed, chain := browser.post(t, tab+"/tracker/connect")

	if !strings.HasSuffix(landed, tab) {
		t.Errorf("Connect left the browser at %q, want it back on %s", landed, tab)
	}

	theRedirectWentThroughTheProvider(t, chain, fixture.server.URL)

	// The token exchange happens after the callback answers, so the tab is what reports the outcome — the same LoginStatus the CLI polls.
	body := theTabEventuallySays(t, browser, tab, "connected")
	if strings.Contains(body, "needs login") {
		t.Errorf("the tab still asks for a login after one finished:\n%s", body)
	}

	theTestButtonProbesTheServer(t, browser, tab, fixture)
}

// The three columns that answer "is this pipeline's tooling wired up" without running a job: what each server is, who depends on it, and what is missing.
func theTabNamesWhatIsWiredUp(t *testing.T, body string) {
	t.Helper()

	for _, want := range []string{
		"tracker", "needs login",
		"keyed", "$TRACKER_PAT",
		"type tracker-items",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the mcp tab never says %q:\n%s", want, body)
		}
	}

	if strings.Contains(body, "a-static-token") {
		t.Error("the mcp tab printed the value of a credential, not the name of the variable holding it")
	}
}

func theRedirectWentThroughTheProvider(t *testing.T, chain []string, provider string) {
	t.Helper()

	var sawAuthorize, sawCallback bool

	for _, hop := range chain {
		sawAuthorize = sawAuthorize || strings.HasPrefix(hop, provider+"/authorize")
		sawCallback = sawCallback || strings.Contains(hop, "/mcp/callback?")
	}

	if !sawAuthorize {
		t.Errorf("Connect never sent the browser to the authorization server; it went %v", chain)
	}

	if !sawCallback {
		t.Errorf("the provider's redirect never reached the daemon's callback; it went %v", chain)
	}
}

// Test is the live half: static status cannot tell an endpoint that moved from one that answers, and this is the only thing on the page that asks.
func theTestButtonProbesTheServer(t *testing.T, browser *browserClient, tab string, fixture *oauthFixture) {
	t.Helper()

	before := fixture.authorized.Load()

	landed, _ := browser.post(t, tab+"/tracker/test")
	if !strings.HasSuffix(landed, tab) {
		t.Errorf("Test left the browser at %q, want it back on %s", landed, tab)
	}

	theTabEventuallySays(t, browser, tab, "1 tool")

	if fixture.authorized.Load() <= before {
		t.Error("the probe never reached the server carrying the token the login saved")
	}
}

// theTabEventuallySays polls the page the way the page polls itself: a probe and a token exchange both finish after the request that started them.
func theTabEventuallySays(t *testing.T, browser *browserClient, tab, want string) string {
	t.Helper()

	var body string

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		body = browser.get(t, tab)
		if strings.Contains(body, want) {
			return body
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("the mcp tab never said %q:\n%s", want, body)

	return body
}

// browserClient is what the Connect button assumes it is talking to: something that follows redirects across origins and sends the daemon's credentials to the daemon.
type browserClient struct {
	client *http.Client
	addr   string
	user   string
	pass   string
	chain  []string
}

func newBrowser(t *testing.T, addr, user, pass string) *browserClient {
	t.Helper()

	browser := &browserClient{addr: addr, user: user, pass: pass}
	browser.client = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			browser.chain = append(browser.chain, req.URL.String())

			if len(via) > 10 {
				return http.ErrUseLastResponse
			}

			return nil
		},
	}

	return browser
}

func (b *browserClient) get(t *testing.T, path string) string {
	t.Helper()

	body, _, _ := b.do(t, http.MethodGet, path)

	return body
}

// post returns where the browser ended up and every hop it took to get there, which is the property under test.
func (b *browserClient) post(t *testing.T, path string) (string, []string) {
	t.Helper()

	b.chain = nil
	_, landed, code := b.do(t, http.MethodPost, path)

	if code != http.StatusOK {
		t.Fatalf("POST %s ended at %s with %d", path, landed, code)
	}

	return landed, b.chain
}

func (b *browserClient) do(t *testing.T, method, path string) (string, string, int) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+b.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth(b.user, b.pass)

	// A browser sends this on every POST, and the daemon reads it for the address to build the redirect URI from — the browser's own answer to "where did you reach me?", which behind a proxy the request itself cannot give.
	if method == http.MethodPost {
		req.Header.Set("Origin", "http://"+b.addr)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return string(body), resp.Request.URL.String(), resp.StatusCode
}
