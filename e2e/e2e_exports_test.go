package e2e

// An export is a seam across the whole stack: the CLI wires sessions, the authorization server and the exporter; the daemon serves an agent's MCP requests; the agent package builds the tools; the upstream receives what they send. So this is one trip through cli.Run with a real MCP client at one end and a fake upstream at the other.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/cli"
)

var consentInput = regexp.MustCompile(`name="consent" value="([^"]+)"`)

// Not t.Parallel(): startWeb backgrounds cli.Run in this process and stop signals the whole test binary.
func TestAnAgentCallsAnExportAsThePersonWhoAgreed(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{
		name:     "search_issues",
		required: []string{"owner", "query"},
		reply: func(args map[string]any) string {
			return "found " + args["query"].(string) + " in " + args["owner"].(string)
		},
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true
mcp_servers:
- name: github
  endpoint: `+upstream.URL+`
mcp_exports:
- name: support
  access: UserID == "alex"
  rate: 2/m
  tools:
  - mcp: github
    tool: search_issues
    args: { owner: acme }
- name: other
  access: "true"
  tools:
  - mcp: github
    tool: search_issues
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", "ops", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	token := connectAgent(t, served.addr, "support")
	session := mcpSession(t, "http://"+served.addr+"/mcp/x/ops/support", token)

	theCallerSeesOnlyWhatIsOffered(t, session)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "github__search_issues", Arguments: map[string]any{"query": "crash", "owner": "evil"}})
	if err != nil || result.IsError {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}

	expectDocMCPCall(t, upstream, "search_issues", map[string]any{"owner": "acme", "query": "crash"})

	theThirdCallIsRefused(t, session)

	_, activity := authBody(t, served.addr, "alex", "a", "/activity?action=mcp+call")
	if !strings.Contains(activity, "alex (local) mcp call support") || strings.Contains(activity, "crash") {
		t.Errorf("the activity log should record alex's calls to support, and never what was asked:\n%s", activity)
	}

	if code := bearerPost(t, "http://"+served.addr+"/mcp/x/ops/other", token); code != http.StatusUnauthorized {
		t.Errorf("support's token at another export = %d, want refused", code)
	}
}

func theThirdCallIsRefused(t *testing.T, session *sdkmcp.ClientSession) {
	t.Helper()

	_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "github__search_issues", Arguments: map[string]any{"query": "again"}})
	if err != nil {
		t.Fatal(err)
	}

	limited, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "github__search_issues", Arguments: map[string]any{"query": "third"}})
	if err != nil || !limited.IsError || !strings.Contains(textOf(limited), "rate limit") {
		t.Errorf("a third call inside 2/m = %+v, %v; want refused as a tool error", limited, err)
	}
}

func theCallerSeesOnlyWhatIsOffered(t *testing.T, session *sdkmcp.ClientSession) {
	t.Helper()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "github__search_issues" {
		t.Fatalf("ListTools = %+v, %v; want only the exported tool", tools, err)
	}

	schema, err := json.Marshal(tools.Tools[0].InputSchema)
	if err != nil || strings.Contains(string(schema), "owner") {
		t.Errorf("the pinned argument is visible to the caller: %s", schema)
	}
}

// connectAgent does what an agent's OAuth client does, with alex's browser in the middle: register, ask, agree, exchange.
func connectAgent(t *testing.T, addr, export string) string {
	t.Helper()

	base := "http://" + addr
	redirect := "http://127.0.0.1:1/callback"

	_, _, cookies := signIn(t, addr, "/")

	client := registerAgent(t, base, redirect)
	verifier := "e2e-verifier-0123456789-0123456789-0123456789-0123456789"
	challenge := pkceChallenge(verifier)

	authorize := "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirect}, "state": {"s"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {base + "/mcp/x/ops/" + export},
	}.Encode()

	_, _, page := browse(t, addr, http.MethodGet, authorize, "", cookies)

	consent := consentInput.FindStringSubmatch(page)
	if len(consent) < 2 {
		t.Fatalf("no consent was asked for:\n%s", page)
	}

	_, location, _ := browse(t, addr, http.MethodPost, "/oauth/authorize", "consent="+consent[1]+"&decision=allow", cookies)

	back, err := url.Parse(location)
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("allowing sent the browser to %q, with no code", location)
	}

	return exchangeCode(t, base, url.Values{"grant_type": {"authorization_code"}, "code": {back.Query().Get("code")}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
}

func registerAgent(t *testing.T, base, redirect string) string {
	t.Helper()

	resp, err := http.Post(base+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"e2e agent","redirect_uris":["`+redirect+`"]}`)) //nolint:noctx // a test's one-shot request
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	var client struct {
		ClientID string `json:"client_id"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&client)

	if resp.StatusCode != http.StatusCreated || client.ClientID == "" {
		t.Fatalf("register = %d", resp.StatusCode)
	}

	return client.ClientID
}

func exchangeCode(t *testing.T, base string, form url.Values) string {
	t.Helper()

	resp, err := http.PostForm(base+"/oauth/token", form) //nolint:noctx // a test's one-shot request
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	var pair struct {
		AccessToken string `json:"access_token"`
	}

	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &pair)

	if pair.AccessToken == "" {
		t.Fatalf("token = %d %s", resp.StatusCode, body)
	}

	return pair.AccessToken
}

// mcpSession is a real MCP client, carrying the token the way an agent does.
func mcpSession(t *testing.T, endpoint, token string) *sdkmcp.ClientSession {
	t.Helper()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "e2e-agent", Version: "v0"}, nil)
	transport := &sdkmcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearer{token: token}, Timeout: 30 * time.Second}}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to %s: %v", endpoint, err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

type bearer struct{ token string }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+b.token)

	return http.DefaultTransport.RoundTrip(clone) //nolint:wrapcheck // a test transport
}

func bearerPost(t *testing.T, endpoint, token string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

func textOf(result *sdkmcp.CallToolResult) string {
	var parts []string

	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}

	return strings.Join(parts, "\n")
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

var clientInput = regexp.MustCompile(`name="client" value="([^"]+)"`)

// Disconnecting an agent ends it at its next call, not at its token's expiry. Not t.Parallel(), for startWeb's reason.
func TestRevokingAConnectionEndsItAtOnce(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{name: "search_issues", required: []string{"query"}, reply: func(map[string]any) string { return "ok" }})

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, "defaults:\n  preflight:\n    disabled: true\nmcp_servers:\n- name: github\n  endpoint: "+upstream.URL+"\nmcp_exports:\n- name: support\n  access: \"true\"\n  tools:\n  - mcp: github\n    tool: search_issues\njobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	defer served.stopIfRunning(t)

	err := cli.Run([]string{"pipeline", "set", "-p", "ops", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	token := connectAgent(t, served.addr, "support")
	session := mcpSession(t, "http://"+served.addr+"/mcp/x/ops/support", token)

	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "github__search_issues", Arguments: map[string]any{"query": "before"}})
	if err != nil {
		t.Fatalf("a call before revoking: %v", err)
	}

	_, _, cookies := signIn(t, served.addr, "/")
	_, _, page := browse(t, served.addr, http.MethodGet, "/connections", "", cookies)

	client := clientInput.FindStringSubmatch(page)
	if len(client) < 2 || !strings.Contains(page, "ops / support") {
		t.Fatalf("the connections page does not list the agent:\n%s", page)
	}

	code, _, _ := browse(t, served.addr, http.MethodPost, "/connections/revoke", url.Values{"client": {client[1]}, "pipeline": {"ops"}, "export": {"support"}}.Encode(), cookies)
	if code != http.StatusSeeOther {
		t.Fatalf("revoke = %d, want 303", code)
	}

	if code := bearerPost(t, "http://"+served.addr+"/mcp/x/ops/support", token); code != http.StatusUnauthorized {
		t.Errorf("the revoked agent's next request = %d, want refused", code)
	}

	if _, _, after := browse(t, served.addr, http.MethodGet, "/connections", "", cookies); strings.Contains(after, "ops / support") {
		t.Error("the revoked agent is still listed")
	}
}
