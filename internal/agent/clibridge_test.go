package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

// bridgeAuth attaches the bridge's bearer token to every request, standing in
// for what the CLI does with the headers in its mcp-config.
type bridgeAuth struct{ token string }

func (a bridgeAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+a.token)

	//nolint:wrapcheck // a test transport, and the caller inspects the original error
	return http.DefaultTransport.RoundTrip(req)
}

// bridgeConversation builds a conversation carrying the given tools, enough
// for the bridge to serve them.
func bridgeConversation(decls []*genai.FunctionDeclaration, registry map[string]toolImpl, required map[string]bool) agentConversation {
	if required == nil {
		required = map[string]bool{}
	}

	return agentConversation{
		env:   toolEnv{dir: "."},
		tools: agentTools{decls: &genai.Tool{FunctionDeclarations: decls}, registry: registry, required: required},
	}
}

// dialBridge connects a real MCP client to a running bridge, the same way the
// child CLI does.
func dialBridge(t *testing.T, bridge *cliBridge) *sdkmcp.ClientSession {
	t.Helper()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-cli", Version: "v0"}, nil)

	// Authenticated the way the child is: the token reaches the real CLI
	// through the mcp-config file.
	transport := &sdkmcp.StreamableClientTransport{
		Endpoint:   bridge.url,
		HTTPClient: &http.Client{Transport: bridgeAuth{token: bridge.token}},
	}

	session, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connecting to bridge: %v", err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

// TestCLIBridgeServesEveryDeclaredTool pins the post-#100 shape: the bridge
// exports EVERY tool in the conversation's registry, builtin included — there
// is no longer a native the CLI serves itself for it to skip.
func TestCLIBridgeServesEveryDeclaredTool(t *testing.T) {
	t.Parallel()

	decls := []*genai.FunctionDeclaration{
		{Name: "read_file", Description: "read", Parameters: &genai.Schema{Type: genai.TypeObject}},
		{Name: "count_lines", Description: "count", Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "the file"}},
			Required:   []string{"path"},
		}},
	}

	registry := map[string]toolImpl{
		"read_file":   func(context.Context, map[string]any, toolEnv) map[string]any { return map[string]any{"exit_code": 0} },
		"count_lines": func(context.Context, map[string]any, toolEnv) map[string]any { return map[string]any{"exit_code": 0} },
	}

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(decls, registry, nil), nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	listed, err := dialBridge(t, bridge).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}

	for _, want := range []string{"read_file", "count_lines"} {
		if !slices.Contains(names, want) {
			t.Errorf("bridged tools = %v, want it to contain %q", names, want)
		}
	}

	// The schema has to survive the genai -> JSON Schema conversion, or the
	// CLI cannot call the tool with the right arguments.
	at := slices.IndexFunc(listed.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "count_lines" })
	if at < 0 {
		t.Fatalf("count_lines missing from %v", names)
	}

	schema, ok := listed.Tools[at].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema is %T, want a JSON object", listed.Tools[at].InputSchema)
	}

	if schema["type"] != "object" {
		t.Errorf(`schema type = %v, want "object"`, schema["type"])
	}

	properties, _ := schema["properties"].(map[string]any)
	if _, has := properties["path"]; !has {
		t.Errorf("schema properties = %v, want a path property", properties)
	}
}

// TestCLIBridgeEnforcesMaxCallsOnAnyTool is issue #100 slice 3's max_calls:
// un-refusal, proven the way TestCLIBridgeEnforcesTheQuestionBudget proves it
// for ask_user's own budget: cliBridge.overBudget does not special-case which
// tool it counts, so an ordinary custom tool's max_calls: binds through the
// same mutex. The (N+1)th call must come back as ordinary tool-result data
// naming the exhausted budget — never an aborted attempt — exactly the
// contract the HTTP path's executeBudgetedTool honours.
func TestCLIBridgeEnforcesMaxCallsOnAnyTool(t *testing.T) {
	t.Parallel()

	var invocations atomic.Int64

	decls := []*genai.FunctionDeclaration{{
		Name: "post_review", Description: "post", Parameters: &genai.Schema{Type: genai.TypeObject},
	}}

	registry := map[string]toolImpl{
		"post_review": func(context.Context, map[string]any, toolEnv) map[string]any {
			invocations.Add(1)

			return map[string]any{"exit_code": 0}
		},
	}

	conv := bridgeConversation(decls, registry, nil)
	conv.tools.maxCalls = map[string]int{"post_review": 1}

	bridge, err := newCLIBridge(t.Context(), conv, nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	session := dialBridge(t, bridge)

	first, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "post_review"})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	if first.IsError {
		t.Errorf("the first call, within budget, came back as an error: %v", first.Content)
	}

	second, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "post_review"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if !second.IsError {
		t.Error("the second call exceeded max_calls: 1 but was not reported as an error")
	}

	if !strings.Contains(bridgeText(t, second), "budget") {
		t.Errorf("the refusal does not name the exhausted budget: %s", bridgeText(t, second))
	}

	// Data, not an abort: the session is still alive to make a third call.
	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "post_review"})
	if err != nil {
		t.Fatalf("a budget-exhausted call must not abort the session: %v", err)
	}

	if invocations.Load() != 1 {
		t.Errorf("the tool impl ran %d times under a budget of 1", invocations.Load())
	}
}

func TestCLIBridgeExecutesAndCapturesVerdict(t *testing.T) {
	t.Parallel()

	decl, impl := buildVerdictTool([]string{"approve", "reject"}, false, stepExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, map[string]bool{verdictToolName: true}),
		nil,
	)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	session := dialBridge(t, bridge)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      verdictToolName,
		Arguments: map[string]any{"choice": "approve", "note": "looks fine"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if result.IsError {
		t.Errorf("a valid verdict came back as an error: %v", result.Content)
	}

	verdict, note, satisfied, calls := bridge.observed()

	if verdict != "approve" || note != "looks fine" {
		t.Errorf("captured verdict/note = %q/%q, want approve/looks fine", verdict, note)
	}

	if !satisfied[verdictToolName] {
		t.Error("the verdict tool was called successfully but is not marked satisfied")
	}

	if len(calls) != 1 || calls[0].name != verdictToolName || !calls[0].ok {
		t.Errorf("recorded calls = %+v, want one successful verdict call", calls)
	}
}

func TestCLIBridgeReportsToolFailureAsError(t *testing.T) {
	t.Parallel()

	decl, impl := buildVerdictTool([]string{"approve"}, false, stepExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, nil),
		nil,
	)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	// Out of the declared vocabulary: the tool returns {"error": ...} data,
	// which must reach the CLI as an MCP error so its model can re-call.
	result, err := dialBridge(t, bridge).CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      verdictToolName,
		Arguments: map[string]any{"choice": "maybe"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if !result.IsError {
		t.Error("an out-of-vocabulary verdict was not reported as an error")
	}

	verdict, _, satisfied, _ := bridge.observed()
	if verdict != "" || satisfied[verdictToolName] {
		t.Error("a failed verdict call was captured as a decision")
	}
}

func TestCLIBridgeWriteConfig(t *testing.T) {
	t.Parallel()

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	path, err := bridge.writeConfig()
	if err != nil {
		t.Fatalf("writeConfig: %v", err)
	}

	// Outside the workspace: the config carries a live callback URL, and the
	// workspace is both captured as artifacts and readable by the agent's own
	// file tools.
	if strings.HasPrefix(path, ".") {
		t.Errorf("mcp config at %q, want it outside the step workspace", path)
	}

	document := struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}{}

	raw, err := os.ReadFile(path) //nolint:gosec // path is the temp file writeConfig just created
	if err != nil {
		t.Fatalf("reading mcp config: %v", err)
	}

	err = json.Unmarshal(raw, &document)
	if err != nil {
		t.Fatalf("mcp config is not valid JSON: %v", err)
	}

	entry, ok := document.MCPServers[cliBridgeServerName]
	if !ok {
		t.Fatalf("mcp config has no %q server: %v", cliBridgeServerName, document.MCPServers)
	}

	if entry.Type != "http" || entry.URL != bridge.url {
		t.Errorf("mcp config entry = %+v, want http at %s", entry, bridge.url)
	}

	// The token travels to the child here and nowhere else. Without it the
	// child cannot call its own tools.
	if entry.Headers["Authorization"] != "Bearer "+bridge.token {
		t.Errorf("mcp config authorization = %q, want the bridge token", entry.Headers["Authorization"])
	}

	assertPrivateFile(t, path)
}

// assertPrivateFile fails unless path is readable only by its owner. The mcp
// config carries a live capability -- a URL plus the token that works on it.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s mode = %v, want no group/other access", path, perm)
	}
}

// TestCLIBridgeRejectsUnauthenticatedCallers is the reason the token exists.
// Loopback is not a permission boundary: any process on the host can reach an
// open localhost port, and what the bridge serves is the step.s custom run:
// tools (arbitrary shell in the workspace) plus the verdict that decides where
// the job goes next.
func TestCLIBridgeRejectsUnauthenticatedCallers(t *testing.T) {
	t.Parallel()

	decl, impl := buildVerdictTool([]string{"approve"}, false, stepExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, nil),
		nil,
	)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"verdict","arguments":{"choice":"approve"}}}`

	for _, tt := range []struct{ name, authorization string }{
		{"no credentials", ""},
		{"a wrong token", "Bearer not-the-token"},
		{"the token without the scheme", bridge.token},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, bridge.url, strings.NewReader(body))
			if err != nil {
				t.Fatalf("building request: %v", err)
			}

			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")

			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("calling the bridge: %v", err)
			}

			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 — an unauthenticated caller reached the tools", resp.StatusCode)
			}
		})
	}

	// Most importantly, nothing it tried was executed.
	if verdict, _, _, calls := bridge.observed(); verdict != "" || len(calls) > 0 {
		t.Errorf("unauthenticated calls were executed: verdict %q, calls %+v", verdict, calls)
	}
}

// TestCLIBridgeAlwaysLoopback pins the post-#100 shape: the CLI process is
// always a host subprocess of this one (see the design note in cliexec.go),
// so the bridge binds loopback unconditionally — there is no longer a
// containerized-CLI case that needs a wider bind to be reachable from.
// `image:` on a step now places the step's TOOLS, never the CLI, and nothing
// inside that container ever dials this bridge: a bridged call arrives here
// from the host-side CLI, and it is THIS process that then runs the tool
// through internal/shell's runner. The container is downstream of the bridge,
// never a client of it.
func TestCLIBridgeAlwaysLoopback(t *testing.T) {
	t.Parallel()

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	if !strings.HasPrefix(bridge.url, "http://127.0.0.1:") {
		t.Errorf("url = %q, want loopback for a host-run cli", bridge.url)
	}
}

// TestCLIBridgeCloseDropsAConnectionThatNeverSentARequest: Shutdown waits on such a connection past its deadline, so Close must drop it itself.
func TestCLIBridgeCloseDropsAConnectionThatNeverSentARequest(t *testing.T) {
	t.Parallel()

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	dial := func() net.Conn {
		var dialer net.Dialer

		conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(bridge.url, "http://"))
		if err != nil {
			t.Fatalf("dialing the bridge: %v", err)
		}

		t.Cleanup(func() { _ = conn.Close() })

		return conn
	}

	silent := dial()

	// Accept is FIFO, so a later connection being served proves the silent one was accepted rather than dropped from the backlog by Close.
	served := dial()
	_, _ = served.Write([]byte("GET / HTTP/1.1\r\nHost: bridge\r\n\r\n"))

	_, err = served.Read(make([]byte, 1))
	if err != nil {
		t.Fatalf("the bridge never answered: %v", err)
	}

	_ = bridge.Close(t.Context())

	_ = silent.SetReadDeadline(time.Now().Add(time.Second))

	_, err = silent.Read(make([]byte, 1))
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("the connection outlived Close — a client holding it open keeps a bridge goroutine alive")
	}
}
