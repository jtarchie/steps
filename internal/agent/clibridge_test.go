package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

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

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: bridge.url}, nil)
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
	bridge, err := newCLIBridge(t.Context(), bridgeConversation(decls, registry, nil), map[string]bool{"read_file": true})
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

	decl, impl := buildVerdictTool([]string{"approve", "reject"})

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

	verdict, note, _, satisfied, calls := bridge.observed()

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

	decl, impl := buildVerdictTool([]string{"approve"})

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

	verdict, _, _, satisfied, _ := bridge.observed()
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
			Type string `json:"type"`
			URL  string `json:"url"`
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
}
