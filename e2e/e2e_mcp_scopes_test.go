package e2e

// A saved login can be perfectly valid and still not be allowed to do the
// work: the server's permission names change under it, or the pipeline's
// auth.scopes: changes and nobody logs in again. tools/list answers either
// way, so preflight said "connected" while every call a model made came back
// 403 — and the model, told that as tool data, kept trying. These tests walk
// what a run sees instead: the refusal ends the step as errored, the next
// preflight says which login and why, and a fresh login clears it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/cli"
)

// scopeFixture is an MCP server behind an authorization server whose scope
// story a test can change mid-test: what it advertises, what a login is
// granted, and whether a tool call is refused for want of a scope.
type scopeFixture struct {
	server *httptest.Server
	// supported is the protected-resource metadata's scopes_supported.
	supported atomic.Value
	// grant is the token response's scope.
	grant atomic.Value
	// refuse makes every tools/call a 403 insufficient_scope.
	refuse atomic.Bool
	// calls counts tools/call requests that reached the server, refused or not.
	calls atomic.Int64
}

const scopeFixtureToken = "scoped-token"

func newScopeFixture(t *testing.T, supported []string, grant string) *scopeFixture {
	t.Helper()

	fixture := &scopeFixture{}
	fixture.supported.Store(supported)
	fixture.grant.Store(grant)

	mux := http.NewServeMux()
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)

	base := fixture.server.URL

	tools := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "scope-fixture", Version: "v0"}, nil)
	tools.AddTool(&sdkmcp.Tool{Name: "read_items", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `[{"id":"ITEM-1"}]`}}}, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return tools }, nil)

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+scopeFixtureToken {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		if bytes.Contains(body, []byte(`"tools/call"`)) {
			fixture.calls.Add(1)

			if fixture.refuse.Load() {
				// RFC 6750 §3.1's challenge, which is what the MCP
				// authorization spec asks a server to send.
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="items:read", error_description="read_items needs items:read"`)
				http.Error(w, "Insufficient scope", http.StatusForbidden)

				return
			}
		}

		handler.ServeHTTP(w, r)
	})

	writeJSON := func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")

		err := json.NewEncoder(w).Encode(body)
		if err != nil {
			t.Errorf("scope fixture: %v", err)
		}
	}

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		supported, _ := fixture.supported.Load().([]string)
		writeJSON(w, map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}, "scopes_supported": supported})
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

		writeJSON(w, map[string]any{"client_id": "scope-client", "redirect_uris": meta["redirect_uris"], "grant_types": meta["grant_types"]})
	})

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		http.Redirect(w, r, query.Get("redirect_uri")+"?code=scope-code&state="+query.Get("state"), http.StatusFound) //nolint:gosec // redirecting to the caller's redirect_uri is what an authorization server IS; this one is a test fixture
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"access_token": scopeFixtureToken, "refresh_token": "scope-refresh", "token_type": "Bearer", "expires_in": 3600}
		if grant, _ := fixture.grant.Load().(string); grant != "" {
			body["scope"] = grant
		}

		writeJSON(w, body)
	})

	return fixture
}

// scopedPipeline grants read_items to an agent on an oauth server, with job
// hooks that leave a file behind so a test can tell errored from failed.
//
// cache is defaults.preflight.cache. The default (5m) is the honest one for a
// check that must not wait out the cache — a refusal, auth.scopes changing —
// and only a check that asks the server itself needs it short.
func scopedPipeline(llm, mcp, scopes, cache, dir string) string {
	auth := "{ type: oauth }"
	if scopes != "" {
		auth = "{ type: oauth, scopes: " + scopes + " }"
	}

	return `
defaults:
  preflight:
    cache: ` + cache + `
mcp_servers:
- name: tracker
  endpoint: ` + mcp + `/mcp
  auth: ` + auth + `
agents:
- name: reader
  source: { model: openai/test-model, endpoint: ` + llm + `/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - mcp: tracker
    tool: read_items
jobs:
- name: read
  plan:
  - agent: reader
    inputs: []
    messages:
      - What items are there?
  on_error:
    task: errored
    run: touch ` + filepath.Join(dir, "errored") + `
  on_failure:
    task: failed
    run: touch ` + filepath.Join(dir, "failed") + `
`
}

// scopeLLM answers a model probe plainly and asks for read_items once per
// conversation, so every request that carries tools is one conversation turn.
func scopeLLM(t *testing.T) *fakeLLM {
	t.Helper()

	return newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if len(req.Tools) == 0 {
			return says("pong")
		}

		if req.historyCalled("tracker__read_items") {
			return says("ITEM-1")
		}

		return callsTool("tracker__read_items", map[string]any{})
	})
}

func toolTurns(fake *fakeLLM) int {
	turns := 0

	for _, req := range fake.allRequests() {
		if len(req.Tools) > 0 {
			turns++
		}
	}

	return turns
}

// scopeTestEnv isolates the token store and $BROWSER, which are process
// environment — the reason none of these tests is parallel.
func scopeTestEnv(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("STEPS_ENCRYPTION_KEY", testEncryptionKey)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")
	t.Setenv("HOME", dir)
	fakeBrowser(t)

	return dir
}

func loginTracker(t *testing.T, path string) {
	t.Helper()

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"mcp", "login", "tracker", "-c", path})
	})
	if err != nil {
		t.Fatalf("steps mcp login tracker: %v\n%s", err, out)
	}
}

func runPipeline(t *testing.T, path string) error {
	t.Helper()

	var err error

	captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--job", "read"})
	})

	return err //nolint:wrapcheck // the test reads the error a user would see, unwrapped
}

func hookFired(t *testing.T, dir, hook string) bool {
	t.Helper()

	_, err := os.Stat(filepath.Join(dir, hook))

	return err == nil
}

func resetHooks(t *testing.T, dir string) {
	t.Helper()

	for _, hook := range []string{"errored", "failed"} {
		_ = os.Remove(filepath.Join(dir, hook))
	}
}

// Not t.Parallel(): the token store and $BROWSER are process environment.
func TestAScopeRefusalErrorsTheStepAndTheNextPreflightSaysSo(t *testing.T) {
	dir := scopeTestEnv(t)
	fixture := newScopeFixture(t, []string{"items:read"}, "items:read")
	fake := scopeLLM(t)
	path := filepath.Join(dir, "p.yml")
	writePipelineFile(t, path, scopedPipeline(fake.URL, fixture.server.URL, "[items:read]", "5m", dir))

	loginTracker(t, path)

	// The login matches everything preflight can see, so the run starts —
	// and the server refuses the call anyway, which is the case no check
	// before the call can rule out.
	fixture.refuse.Store(true)

	theRefusalErroredTheStep(t, runPipeline(t, path), dir, fake)

	resetHooks(t, dir)

	callsBefore := fixture.calls.Load()
	thePreflightRefusedTheRun(t, runPipeline(t, path), dir)

	if toolTurns(fake) != 1 || fixture.calls.Load() != callsBefore {
		t.Errorf("preflight let the run reach the model (%d turns) or the server (%d calls before, %d after)",
			toolTurns(fake), callsBefore, fixture.calls.Load())
	}

	// Somebody logs in again with the scope fixed. That clears it: the
	// refusal belonged to the login that earned it.
	fixture.refuse.Store(false)
	loginTracker(t, path)

	err := runPipeline(t, path)
	if err != nil {
		t.Fatalf("the run after a fresh login: %v", err)
	}
}

// theRefusalErroredTheStep: the error names everything needed to fix it,
// on_error fired alone, and the conversation ended at the refused call — a
// model handed the 403 as data spends turns retrying a call only a login fixes.
func theRefusalErroredTheStep(t *testing.T, err error, dir string, fake *fakeLLM) {
	t.Helper()

	if err == nil {
		t.Fatal("a run whose only tool call was refused for want of a scope succeeded")
	}

	for _, want := range []string{`"tracker"`, "read_items", `"items:read"`, "steps mcp login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("run error = %q, want it to name %s", err, want)
		}
	}

	if !hookFired(t, dir, "errored") || hookFired(t, dir, "failed") {
		t.Errorf("errored hook fired = %v, failed hook fired = %v; want on_error alone: the machinery said no, not the step",
			hookFired(t, dir, "errored"), hookFired(t, dir, "failed"))
	}

	if turns := toolTurns(fake); turns != 1 {
		t.Errorf("the model was asked %d turn(s), want 1", turns)
	}
}

// thePreflightRefusedTheRun: the next run is refused before a turn or a call,
// naming what was refused, and still fires on_error.
func thePreflightRefusedTheRun(t *testing.T, err error, dir string) {
	t.Helper()

	if err == nil || !strings.Contains(err.Error(), "preflight failed") || !strings.Contains(err.Error(), "read_items") {
		t.Fatalf("the run after a refusal = %v, want preflight to refuse it naming read_items", err)
	}

	if !hookFired(t, dir, "errored") {
		t.Error("a preflight refusal fired no on_error")
	}
}

// Not t.Parallel(): the token store and $BROWSER are process environment.
func TestPreflightNoticesTheServerRenamedItsScopes(t *testing.T) {
	dir := scopeTestEnv(t)
	fixture := newScopeFixture(t, []string{"items:read"}, "items:read")
	fake := scopeLLM(t)
	path := filepath.Join(dir, "p.yml")
	// No auth.scopes: the login asks for everything, and what it was
	// granted is the only record of what it holds.
	writePipelineFile(t, path, scopedPipeline(fake.URL, fixture.server.URL, "", "1ms", dir))

	loginTracker(t, path)

	err := runPipeline(t, path)
	if err != nil {
		t.Fatalf("a run over a login the server still recognizes: %v", err)
	}

	fixture.supported.Store([]string{"content:read", "content:write"})

	err = runPipeline(t, path)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("a run over a login granted only scopes the server stopped offering = %v, want preflight to refuse it", err)
	}

	for _, want := range []string{`"items:read"`, `"content:read"`, "steps mcp login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("preflight error = %q, want it to name %s", err, want)
		}
	}
}

// Not t.Parallel(): the token store and $BROWSER are process environment.
func TestPreflightNoticesAuthScopesChangedSinceTheLogin(t *testing.T) {
	dir := scopeTestEnv(t)
	fixture := newScopeFixture(t, []string{"items:read", "items:write"}, "")
	fake := scopeLLM(t)
	path := filepath.Join(dir, "p.yml")
	writePipelineFile(t, path, scopedPipeline(fake.URL, fixture.server.URL, "[items:read]", "5m", dir))

	loginTracker(t, path)

	err := runPipeline(t, path)
	if err != nil {
		t.Fatalf("a run over a login made with the pipeline's own scopes: %v", err)
	}

	writePipelineFile(t, path, scopedPipeline(fake.URL, fixture.server.URL, "[items:read, items:write]", "5m", dir))

	err = runPipeline(t, path)
	if err == nil || !strings.Contains(err.Error(), "preflight failed") || !strings.Contains(err.Error(), `"items:write"`) {
		t.Fatalf("a run after auth.scopes gained items:write = %v, want preflight to refuse it naming the new scope", err)
	}

	loginTracker(t, path)

	err = runPipeline(t, path)
	if err != nil {
		t.Fatalf("the run after logging in with the new scopes: %v", err)
	}
}

// A resource's MCP check reaches a login the same way an agent does, through
// its own preflight cache, so it is held to the same check.
//
// Not t.Parallel(): the token store and $BROWSER are process environment.
func TestAResourceCheckNoticesAuthScopesChangedSinceTheLogin(t *testing.T) {
	dir := scopeTestEnv(t)
	fixture := newScopeFixture(t, []string{"items:read", "items:write"}, "")
	path := filepath.Join(dir, "p.yml")
	ran := filepath.Join(dir, "ran.log")

	pipeline := func(scopes string) string {
		return `
mcp_servers:
- name: tracker
  endpoint: ` + fixture.server.URL + `/mcp
  auth: { type: oauth, scopes: ` + scopes + ` }
resource_types:
- name: tracker-items
  config:
    mcp:
      server: tracker
      check:
        tool: read_items
resources:
- name: items
  type: tracker-items
  source: {}
jobs:
- name: read
  plan:
  - get: items
  - task: record
    inputs: [items]
    run: cat items/version.json >> ` + ran + `
`
	}

	writePipelineFile(t, path, pipeline("[items:read]"))
	loginTracker(t, path)

	err := runPipeline(t, path)
	if err != nil {
		t.Fatalf("a check over a login made with the pipeline's own scopes: %v", err)
	}

	writePipelineFile(t, path, pipeline("[items:read, items:write]"))

	err = runPipeline(t, path)
	if err == nil || !strings.Contains(err.Error(), `"items:write"`) {
		t.Fatalf("a check after auth.scopes gained items:write = %v, want it refused naming the new scope", err)
	}
}
