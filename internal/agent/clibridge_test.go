package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/shell"
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

func TestCLIBridgeServesNonNativeTools(t *testing.T) {
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

	// read_file is served by the CLI natively, so the bridge must not
	// re-export it — offering the same capability twice under two names would
	// leave the model choosing between them.
	bridge, err := newCLIBridge(t.Context(), bridgeConversation(decls, registry, nil), map[string]bool{"read_file": true}, reachHost)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	listed, err := dialBridge(t, bridge).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(listed.Tools) != 1 || listed.Tools[0].Name != "count_lines" {
		names := make([]string, 0, len(listed.Tools))
		for _, tool := range listed.Tools {
			names = append(names, tool.Name)
		}

		t.Fatalf("bridged tools = %v, want [count_lines]", names)
	}

	// The schema has to survive the genai -> JSON Schema conversion, or the
	// CLI cannot call the tool with the right arguments.
	schema, ok := listed.Tools[0].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("input schema is %T, want a JSON object", listed.Tools[0].InputSchema)
	}

	if schema["type"] != "object" {
		t.Errorf(`schema type = %v, want "object"`, schema["type"])
	}

	properties, _ := schema["properties"].(map[string]any)
	if _, has := properties["path"]; !has {
		t.Errorf("schema properties = %v, want a path property", properties)
	}
}

func TestCLIBridgeExecutesAndCapturesVerdict(t *testing.T) {
	t.Parallel()

	decl, impl := buildVerdictTool([]string{"approve", "reject"}, false, assertFilesExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, map[string]bool{verdictToolName: true}),
		nil,
		reachHost,
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

	decl, impl := buildVerdictTool([]string{"approve"}, false, assertFilesExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, nil),
		nil,
		reachHost,
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

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil, reachHost)
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

	decl, impl := buildVerdictTool([]string{"approve"}, false, assertFilesExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, nil),
		nil,
		reachHost,
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

// TestCLIBridgeContainerizedIsReachableFromAContainer covers the two things a
// containerized child needs that a host child does not: a bind it can
// actually reach (a container is not on the host's loopback), and a URL
// naming the host rather than the wildcard address that bind produced.
func TestCLIBridgeContainerizedIsReachableFromAContainer(t *testing.T) {
	t.Parallel()

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil, reachGateway)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	if !strings.HasPrefix(bridge.url, "http://"+shell.HostGatewayName+":") {
		t.Errorf("url = %q, want it to name %s", bridge.url, shell.HostGatewayName)
	}

	// A wildcard bind is not a destination: if the URL kept it, every bridged
	// tool call from the container would go nowhere.
	if strings.Contains(bridge.url, "0.0.0.0") {
		t.Errorf("url = %q, must not hand the child the wildcard address", bridge.url)
	}

	host, _, err := net.SplitHostPort(bridge.listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	if host == "127.0.0.1" {
		t.Errorf("listener bound %q, which a container cannot reach", host)
	}
}

// TestCLIBridgeHostPathStaysLoopback is the other half: nothing widens for a
// step that never asked for a container.
func TestCLIBridgeHostPathStaysLoopback(t *testing.T) {
	t.Parallel()

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil, reachHost)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	if !strings.HasPrefix(bridge.url, "http://127.0.0.1:") {
		t.Errorf("url = %q, want loopback for a host-run cli", bridge.url)
	}
}
