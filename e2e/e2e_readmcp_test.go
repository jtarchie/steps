package e2e

// The read-only MCP server at /mcp crosses the whole stack: the CLI wires sessions and --role, the daemon runs a job and records it, the authorization server issues a read token to a person, and an MCP client reads the run back as them. What it must never do is let that person's agent see more than their browser would, so every test here is about a boundary.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// failingPipeline prints more than one page of output and then fails.
const failingPipeline = `
jobs:
- name: build
  plan:
  - task: compile
    run: |
      i=0
      while [ $i -lt 1500 ]; do echo "line $i of the build log"; i=$((i+1)); done
      exit 3
`

// hiddenPipeline is seen by alex and nobody else.
const hiddenPipeline = `
access:
  viewer: UserID == "alex"
jobs:
- name: deploy
  plan:
  - task: ship
    run: echo shipped
`

// readDaemon is a daemon with alex (admin) and bob (viewer), serving ops to both and secret to alex alone.
func readDaemon(t *testing.T, extra ...string) *webProcess {
	t.Helper()

	dir := t.TempDir()
	ops, secret := filepath.Join(dir, "ops.yml"), filepath.Join(dir, "secret.yml")
	writePipelineFile(t, ops, failingPipeline)
	writePipelineFile(t, secret, hiddenPipeline)

	state := filepath.Join(dir, "daemon.db")
	args := append([]string{
		"--db", state,
		"--local-user", "alex:" + hashed(t, "a"), "--local-user", "bob:" + hashed(t, "b"),
		"--role", `admin=UserID == "alex"`, "--role", `viewer=UserID == "alex" || UserID == "bob"`,
	}, extra...)

	served := startWeb(t, args...)
	served.state = state

	for name, path := range map[string]string{"ops": ops, "secret": secret} {
		err := stepsRun(t, []string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
		if err != nil {
			t.Fatalf("steps pipeline set %s: %v", name, err)
		}
	}

	return served
}

// failedBuild triggers ops/build as alex and waits for it to fail.
func failedBuild(t *testing.T, served *webProcess) string {
	t.Helper()

	err := stepsRun(t, []string{"runs", "trigger", "build", "-p", "ops", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps runs trigger: %v", err)
	}

	run := newestRun(t, served.state, "ops", "build")
	waitForRunStatus(t, served.state, "ops", run.ID, "failed")

	return run.ID
}

func TestAnAgentReadsWhatItsPersonCanSee(t *testing.T) {
	t.Parallel()

	served := readDaemon(t)
	defer served.stopIfRunning(t)

	runID := failedBuild(t, served)

	pair := connectReader(t, served.addr, "bob", "b")
	session := mcpSession(t, "http://"+served.addr+"/mcp", pair.access)

	assertTools(t, session)

	theHiddenPipelineIsNotThere(t, session)

	failed := failedStepOf(t, session, runID)

	theOutputComesInPages(t, session, runID, failed)

	_, activity := authBody(t, served.addr, "alex", "a", "/activity?action=mcp+read")
	if !strings.Contains(activity, "bob (local) mcp read get_step_output") || strings.Contains(activity, runID) {
		t.Errorf("the activity log should record bob's reads by tool, and never what was asked:\n%s", activity)
	}
}

// theHiddenPipelineIsNotThere: secret is missing from bob's list, and asking for it by name is the not-found a name nobody set gets.
func theHiddenPipelineIsNotThere(t *testing.T, session *sdkmcp.ClientSession) {
	t.Helper()

	var pipelines struct {
		Pipelines []struct{ Name string } `json:"pipelines"`
	}

	readTool(t, session, "list_pipelines", nil, &pipelines)

	if len(pipelines.Pipelines) != 1 || pipelines.Pipelines[0].Name != "ops" {
		t.Errorf("bob's agent lists %+v; want ops alone, with secret hidden", pipelines.Pipelines)
	}

	for tool, args := range map[string]map[string]any{
		"get_pipeline": {"pipeline": "secret"},
		"list_runs":    {"pipeline": "secret"},
		"get_config":   {"pipeline": "secret"},
	} {
		if text := refusedTool(t, session, tool, args); !strings.Contains(text, "no such") || strings.Contains(text, "access") {
			t.Errorf("%s on a pipeline hidden from bob = %q; want the not-found a name nobody set gets", tool, text)
		}
	}
}

// failedStepOf is the step get_run names as the one that failed, which must be compile.
func failedStepOf(t *testing.T, session *sdkmcp.ClientSession, runID string) int64 {
	t.Helper()

	var run struct {
		Status     string `json:"status"`
		FailedStep *struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"failed_step"`
	}

	// The run row says failed a moment before the bus has recorded the step's own end, so wait for that rather than read a step the record has not closed yet.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		readTool(t, session, "get_run", map[string]any{"pipeline": "ops", "run": runID}, &run)

		if run.FailedStep != nil {
			break
		}
	}

	if run.Status != "failed" || run.FailedStep == nil || run.FailedStep.Name != "compile" || run.FailedStep.Status != "failed" {
		t.Fatalf("get_run = %+v; want the run failed at compile", run)
	}

	return run.FailedStep.ID
}

func assertTools(t *testing.T, session *sdkmcp.ClientSession) {
	t.Helper()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"get_config", "get_cost", "get_pipeline", "get_run", "get_step_output", "list_pipelines", "list_resources", "list_runs", "list_versions", "list_waiting", "search"}
	got := make([]string, 0, len(tools.Tools))

	for _, tool := range tools.Tools {
		got = append(got, tool.Name)

		if a := tool.Annotations; a == nil || !a.ReadOnlyHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s is not marked read-only and closed-world: %+v", tool.Name, a)
		}
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// theOutputComesInPages reads a step's output page by page and requires the pages to be the whole of it, in order, none of them all of it.
func theOutputComesInPages(t *testing.T, session *sdkmcp.ClientSession, runID string, step int64) {
	t.Helper()

	var (
		whole  strings.Builder
		offset int
		pages  int
	)

	for {
		var page struct {
			Text       string `json:"text"`
			Offset     int    `json:"offset"`
			NextOffset int    `json:"next_offset"`
			Total      int    `json:"total_bytes"`
		}

		readTool(t, session, "get_step_output", map[string]any{"pipeline": "ops", "run": runID, "step": step, "offset": offset}, &page)

		if page.Offset != offset || len(page.Text) == 0 {
			t.Fatalf("page at %d = offset %d, %d bytes", offset, page.Offset, len(page.Text))
		}

		whole.WriteString(page.Text)
		pages++

		if page.NextOffset == 0 {
			if whole.Len() != page.Total {
				t.Errorf("the pages add up to %d bytes, and the step printed %d", whole.Len(), page.Total)
			}

			break
		}

		offset = page.NextOffset
	}

	if pages < 2 {
		t.Errorf("a %d-byte log came back in %d page; want it paged", whole.Len(), pages)
	}

	lines := strings.Split(strings.TrimSpace(whole.String()), "\n")
	if len(lines) != 1500 || lines[0] != "line 0 of the build log" || lines[1499] != "line 1499 of the build log" {
		t.Errorf("the paged output is %d lines, from %q to %q", len(lines), lines[0], lines[len(lines)-1])
	}
}

// Each token works only where it was issued for: a read token is refused at an export and at /api, an export's token at /mcp, and a refresh mints a pair that still reads.
func TestAReadTokenWorksOnlyAtTheReadServer(t *testing.T) {
	t.Parallel()

	upstream := startDocMCPServer(t, docMCPTool{name: "search_issues", required: []string{"query"}, reply: func(map[string]any) string { return "ok" }})

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, "defaults:\n  preflight:\n    disabled: true\nmcp_servers:\n- name: github\n  endpoint: "+upstream.URL+"\nmcp_exports:\n- name: support\n  access: \"true\"\n  tools:\n  - mcp: github\n    tool: search_issues\njobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	defer served.stopIfRunning(t)

	err := stepsRun(t, []string{"pipeline", "set", "-p", "ops", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	read := connectReader(t, served.addr, "alex", "a")
	export := connectAgent(t, served.addr, "support")
	base := "http://" + served.addr

	for _, refused := range []struct {
		what, endpoint, token string
	}{
		{"a read token at an export", base + "/mcp/x/ops/support", read.access},
		{"an export's token at /mcp", base + "/mcp", export},
		{"a read token's refresh half at /mcp", base + "/mcp", read.refresh},
	} {
		if code := bearerPost(t, refused.endpoint, refused.token); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", refused.what, code)
		}
	}

	if code := bearerGet(t, base+"/api/pipelines", read.access); code != http.StatusUnauthorized {
		t.Errorf("a read token at /api = %d, want 401", code)
	}

	if code := readPost(t, base+"/mcp", read.access); code != http.StatusOK {
		t.Errorf("the read token at /mcp = %d, want 200", code)
	}

	next := refreshPair(t, base, read)
	if code := readPost(t, base+"/mcp", next.access); code != http.StatusOK {
		t.Errorf("the refreshed read token at /mcp = %d, want 200", code)
	}

	if code := bearerPost(t, base+"/mcp/x/ops/support", next.access); code != http.StatusUnauthorized {
		t.Errorf("the refreshed read token at an export = %d, want 401", code)
	}
}

// Disconnecting a reader from the connections page ends it at its next call.
func TestRevokingAReaderEndsItAtOnce(t *testing.T) {
	t.Parallel()

	served := readDaemon(t)
	defer served.stopIfRunning(t)

	pair := connectReader(t, served.addr, "bob", "b")
	base := "http://" + served.addr

	if code := readPost(t, base+"/mcp", pair.access); code != http.StatusOK {
		t.Fatalf("before revoking = %d", code)
	}

	cookies := signInAs(t, served.addr, "bob", "b")
	_, _, page := browse(t, served.addr, http.MethodGet, "/connections", "", cookies)

	client := clientInput.FindStringSubmatch(page)
	if len(client) < 2 || !strings.Contains(page, "reads every pipeline you can see") {
		t.Fatalf("the connections page does not list the reader:\n%s", page)
	}

	code, _, _ := browse(t, served.addr, http.MethodPost, "/connections/revoke", url.Values{"client": {client[1]}, "kind": {"read"}}.Encode(), cookies)
	if code != http.StatusSeeOther {
		t.Fatalf("revoke = %d, want 303", code)
	}

	if code := bearerPost(t, base+"/mcp", pair.access); code != http.StatusUnauthorized {
		t.Errorf("the revoked reader's next request = %d, want 401", code)
	}

	if refreshRefused(t, base, pair) == "" {
		t.Error("the revoked reader could still refresh")
	}
}

// A set that narrows a pipeline takes it from an agent already connected, at its very next call.
func TestAPipelineNarrowedBySetIsHiddenAtTheNextCall(t *testing.T) {
	t.Parallel()

	served := readDaemon(t)
	defer served.stopIfRunning(t)

	session := mcpSession(t, "http://"+served.addr+"/mcp", connectReader(t, served.addr, "bob", "b").access)

	readTool(t, session, "get_pipeline", map[string]any{"pipeline": "ops"}, &struct{}{})

	narrowed := filepath.Join(t.TempDir(), "ops.yml")
	writePipelineFile(t, narrowed, "access:\n  viewer: UserID == \"alex\"\n"+failingPipeline)

	err := stepsRun(t, []string{"pipeline", "set", "-p", "ops", "-c", narrowed, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	if text := refusedTool(t, session, "get_pipeline", map[string]any{"pipeline": "ops"}); !strings.Contains(text, "no such pipeline") {
		t.Errorf("get_pipeline after the set narrowed ops = %q; want not found", text)
	}

	var pipelines struct {
		Pipelines []struct{ Name string } `json:"pipelines"`
	}

	readTool(t, session, "list_pipelines", nil, &pipelines)

	if len(pipelines.Pipelines) != 0 {
		t.Errorf("list_pipelines after the set = %+v; want nothing", pipelines.Pipelines)
	}
}

// --mcp-access is a second rule, apart from the viewer role: bob may read the pages and still not connect an agent.
func TestMCPAccessDecidesWhoMayConnectAnAgent(t *testing.T) {
	t.Parallel()

	served := readDaemon(t, "--mcp-access", `UserID == "alex"`)
	defer served.stopIfRunning(t)

	if code, _ := authBody(t, served.addr, "bob", "b", "/p/ops"); code != http.StatusOK {
		t.Fatalf("bob reads the page = %d; the rule is about agents, not pages", code)
	}

	code, page := authorizeRead(t, served.addr, "bob", "b")
	if code != http.StatusForbidden || !strings.Contains(page, "You may not connect an agent to read this daemon") || consentInput.MatchString(page) {
		t.Errorf("bob's consent page = %d, offering consent: %v\n%s", code, consentInput.MatchString(page), page)
	}

	session := mcpSession(t, "http://"+served.addr+"/mcp", connectReader(t, served.addr, "alex", "a").access)
	readTool(t, session, "list_pipelines", nil, &struct{}{})
}

// A daemon nobody signs in to serves the read server with no sign-in, the way it serves its pages, and --read-only, which withholds acting, still offers reading.
func TestALocalDaemonServesReadsWithNoSignIn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, failingPipeline)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--read-only")
	defer served.stopIfRunning(t)

	served.set(t, "ops", path)

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "e2e-agent", Version: "v0"}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: "http://" + served.addr + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect with no token: %v", err)
	}

	defer func() { _ = session.Close() }()

	var pipelines struct {
		Pipelines []struct{ Name string } `json:"pipelines"`
	}

	readTool(t, session, "list_pipelines", nil, &pipelines)

	if len(pipelines.Pipelines) != 1 || pipelines.Pipelines[0].Name != "ops" {
		t.Errorf("list_pipelines = %+v; want ops", pipelines.Pipelines)
	}
}

// readTool calls a tool that must answer, decoding its structured content into out.
func readTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError {
		t.Fatalf("%s(%v) = %v, %v", name, args, textOf(orEmpty(result)), err)
	}

	if !strings.HasPrefix(textOf(result), "Read-only data from a steps daemon.") {
		t.Errorf("%s's text is not framed as data: %.120q", name, textOf(result))
	}

	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(encoded, out)
	if err != nil {
		t.Fatalf("%s: %v in %s", name, err, encoded)
	}
}

func orEmpty(result *sdkmcp.CallToolResult) *sdkmcp.CallToolResult {
	if result == nil {
		return &sdkmcp.CallToolResult{}
	}

	return result
}

// refusedTool calls a tool that must answer with a tool error, returning what it said.
func refusedTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) string {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || !result.IsError {
		t.Fatalf("%s(%v) = %+v, %v; want a tool error", name, args, result, err)
	}

	return textOf(result)
}

type tokenPair struct{ access, refresh, client string }

func signInAs(t *testing.T, addr, user, pass string) []*http.Cookie {
	t.Helper()

	client, req := browserRequest(t, addr, http.MethodPost, "/login", url.Values{"username": {user}, "password": {pass}, "next": {"/"}}.Encode(), nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.Cookies()
}

const readRedirect = "http://127.0.0.1:1/callback"

// readAuthorizeURL is what an agent sends a person to, asking for the read server.
func readAuthorizeURL(addr, client string) string {
	return "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {client}, "redirect_uri": {readRedirect}, "state": {"s"},
		"code_challenge": {pkceChallenge(readVerifier)}, "code_challenge_method": {"S256"}, "resource": {"http://" + addr + "/mcp"},
	}.Encode()
}

const readVerifier = "e2e-read-verifier-0123456789-0123456789-0123456789-0123456789"

// authorizeRead is the consent page a person is shown for a fresh client.
func authorizeRead(t *testing.T, addr, user, pass string) (int, string) {
	t.Helper()

	cookies := signInAs(t, addr, user, pass)
	client := registerAgent(t, "http://"+addr, readRedirect)
	code, _, page := browse(t, addr, http.MethodGet, readAuthorizeURL(addr, client), "", cookies)

	return code, page
}

// connectReader is an agent's OAuth client connecting to /mcp with a person's browser in the middle: register, ask, agree, exchange.
func connectReader(t *testing.T, addr, user, pass string) tokenPair {
	t.Helper()

	base := "http://" + addr
	cookies := signInAs(t, addr, user, pass)
	client := registerAgent(t, base, readRedirect)

	_, _, page := browse(t, addr, http.MethodGet, readAuthorizeURL(addr, client), "", cookies)

	consent := consentInput.FindStringSubmatch(page)
	if len(consent) < 2 || !strings.Contains(page, "read every pipeline you can see, as you.") {
		t.Fatalf("no read consent was asked for:\n%s", page)
	}

	_, location, _ := browse(t, addr, http.MethodPost, "/oauth/authorize", "consent="+consent[1]+"&decision=allow", cookies)

	back, err := url.Parse(location)
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("allowing sent the browser to %q, with no code", location)
	}

	pair := tokenRequest(t, base, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client}, "redirect_uri": {readRedirect}, "code_verifier": {readVerifier}})
	pair.client = client

	return pair
}

func refreshPair(t *testing.T, base string, pair tokenPair) tokenPair {
	t.Helper()

	next := tokenRequest(t, base, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair.refresh}, "client_id": {pair.client}})
	next.client = pair.client

	return next
}

// refreshRefused is the OAuth error a refresh was refused with, or "" if it was not.
func refreshRefused(t *testing.T, base string, pair tokenPair) string {
	t.Helper()

	status, body := postToken(t, base, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair.refresh}, "client_id": {pair.client}})
	if status == http.StatusOK {
		return ""
	}

	return body
}

func tokenRequest(t *testing.T, base string, form url.Values) tokenPair {
	t.Helper()

	status, body := postToken(t, base, form)

	var pair struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}

	_ = json.Unmarshal([]byte(body), &pair)

	if status != http.StatusOK || pair.AccessToken == "" {
		t.Fatalf("token = %d %s", status, body)
	}

	return tokenPair{access: pair.AccessToken, refresh: pair.RefreshToken}
}

func postToken(t *testing.T, base string, form url.Values) (int, string) {
	t.Helper()

	resp, err := http.PostForm(base+"/oauth/token", form) //nolint:noctx // a test's one-shot request
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

func bearerGet(t *testing.T, endpoint, token string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// readPost is one tools/list the way an MCP client sends it, so a 200 means the request was answered and not merely let through.
func readPost(t *testing.T, endpoint, token string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}
