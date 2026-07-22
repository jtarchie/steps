package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

// countingMCPServer builds an httptest.Server exposing two tools
// ("search_issues", "get_issue") via a real MCP server, counting how many
// times a client issues a tools/list request — used to prove buildMCPTools
// shares one connection (and therefore one ListTools call) across every
// tool a single spec selects, rather than connecting per tool.
type countingMCPServer struct {
	ts         *httptest.Server
	listCalls  *int
	echoCalled *[]map[string]any
}

func newCountingMCPServer(t *testing.T) countingMCPServer {
	t.Helper()

	listCalls := 0
	echoCalled := []map[string]any{}

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	handler := func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args map[string]any

		_ = json.Unmarshal(req.Params.Arguments, &args)
		echoCalled = append(echoCalled, args)

		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}},
			StructuredContent: map[string]any{"ok": true},
		}, nil
	}

	srv.AddTool(&sdkmcp.Tool{
		Name:        "search_issues",
		Description: "Search issues.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
	}, handler)

	srv.AddTool(&sdkmcp.Tool{
		Name:        "get_issue",
		Description: "Get one issue.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
	}, handler)

	countingHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", countingListTools(countingHandler, &listCalls))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return countingMCPServer{ts: ts, listCalls: &listCalls, echoCalled: &echoCalled}
}

func (s countingMCPServer) server() config.MCPServer {
	return config.MCPServer{Name: "test", Endpoint: s.ts.URL + "/mcp"}
}

// countingListTools wraps handler, incrementing *count whenever the request
// body's method is "tools/list" — a lightweight sniff good enough for this
// test without decoding the full JSON-RPC envelope.
func countingListTools(handler http.Handler, count *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				_ = r.Body.Close()

				if bytes.Contains(body, []byte(`"tools/list"`)) {
					*count++
				}

				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}

		handler.ServeHTTP(w, r)
	})
}

func TestBuildMCPToolsSingleForm(t *testing.T) {
	t.Parallel()

	srv := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{srv.server()}}

	decls, registry, closer, err := buildAgentTools(context.Background(), cfg, []config.ToolSpec{{MCP: "test", MCPTool: "search_issues", Description: "override"}}, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}
	defer closeAll(closer)

	if len(decls.FunctionDeclarations) != 1 {
		t.Fatalf("declarations = %+v, want 1", decls.FunctionDeclarations)
	}

	decl := decls.FunctionDeclarations[0]
	if decl.Name != "test__search_issues" {
		t.Errorf("Name = %q, want test__search_issues", decl.Name)
	}

	if decl.Description != "override" {
		t.Errorf("Description = %q, want the spec override", decl.Description)
	}

	if decl.ParametersJsonSchema == nil {
		t.Error("ParametersJsonSchema is nil, want the server's advertised schema")
	}

	impl, ok := registry["test__search_issues"]
	if !ok {
		t.Fatal("registry missing test__search_issues")
	}

	result := impl(context.Background(), map[string]any{"query": "bug"}, toolEnv{})
	if result["error"] != nil {
		t.Fatalf("call result = %+v, want no error", result)
	}
}

func TestBuildMCPToolsSubsetAndAllShareOneConnection(t *testing.T) {
	t.Parallel()

	srv := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{srv.server()}}

	_, registry, closer, err := buildAgentTools(context.Background(), cfg, []config.ToolSpec{{MCP: "test"}}, "") // bare form: all tools
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}
	defer closeAll(closer)

	if len(registry) != 2 {
		t.Fatalf("registry = %+v, want 2 tools (search_issues, get_issue)", registry)
	}

	if _, ok := registry["test__search_issues"]; !ok {
		t.Error("registry missing test__search_issues")
	}

	if _, ok := registry["test__get_issue"]; !ok {
		t.Error("registry missing test__get_issue")
	}

	if *srv.listCalls != 1 {
		t.Errorf("ListTools called %d times, want exactly 1 (one connection shared across both selected tools)", *srv.listCalls)
	}
}

func TestBuildMCPToolsUnknownToolName(t *testing.T) {
	t.Parallel()

	srv := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{srv.server()}}

	_, _, closer, err := buildAgentTools(context.Background(), cfg, []config.ToolSpec{{MCP: "test", MCPTool: "ghost_tool"}}, "")
	if err == nil {
		closeAll(closer)
		t.Fatal("buildAgentTools: expected an error for an unknown mcp tool name")
	}
}

func TestBuildMCPToolsCloserClosesConnection(t *testing.T) {
	t.Parallel()

	srv := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{srv.server()}}

	_, _, closer, err := buildAgentTools(context.Background(), cfg, []config.ToolSpec{{MCP: "test", MCPTool: "search_issues"}}, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}

	closeAll(closer)
	closeAll(closer) // must not panic or error on a second close
}

// fakeMCPClient is a hand-rolled stepsmcp.Client double for testing
// mcpToolImpl's result-translation branches in isolation, without a real
// server.
type fakeMCPClient struct {
	result   *sdkmcp.CallToolResult
	err      error
	gotArgs  map[string]any
	closeErr error
	closed   bool
}

func (f *fakeMCPClient) ListTools(context.Context) ([]*sdkmcp.Tool, error) { return nil, nil }

func (f *fakeMCPClient) CallTool(_ context.Context, _ string, args map[string]any) (*sdkmcp.CallToolResult, error) {
	f.gotArgs = args

	return f.result, f.err
}

func (f *fakeMCPClient) Close() error {
	f.closed = true

	return f.closeErr
}

var _ stepsmcp.Client = (*fakeMCPClient)(nil)

func TestMCPToolImplSuccess(t *testing.T) {
	t.Parallel()

	client := &fakeMCPClient{result: &sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: "line one"}, &sdkmcp.TextContent{Text: "line two"}},
		StructuredContent: map[string]any{"id": "42"},
	}}

	impl := mcpToolImpl(client, "search_issues")
	result := impl(context.Background(), map[string]any{"query": "bug"}, toolEnv{})

	if result["content"] != "line one\nline two" {
		t.Errorf("content = %#v", result["content"])
	}

	sc, ok := result["structured_content"].(map[string]any)
	if !ok || sc["id"] != "42" {
		t.Errorf("structured_content = %#v", result["structured_content"])
	}

	if _, hasErr := result["error"]; hasErr {
		t.Errorf("result = %+v, want no error key", result)
	}

	if client.gotArgs["query"] != "bug" {
		t.Errorf("CallTool args = %+v, want query=bug forwarded", client.gotArgs)
	}
}

func TestMCPToolImplIsError(t *testing.T) {
	t.Parallel()

	client := &fakeMCPClient{result: &sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "issue not found"}},
		IsError: true,
	}}

	impl := mcpToolImpl(client, "get_issue")
	result := impl(context.Background(), map[string]any{"id": "999"}, toolEnv{})

	if result["error"] != "issue not found" {
		t.Errorf("error = %#v, want %q", result["error"], "issue not found")
	}

	if _, hasContent := result["content"]; hasContent {
		t.Errorf("result = %+v, an IsError result must not carry content/structured_content keys", result)
	}
}

func TestMCPToolImplTransportError(t *testing.T) {
	t.Parallel()

	client := &fakeMCPClient{err: errors.New("connection reset")}

	impl := mcpToolImpl(client, "search_issues")
	result := impl(context.Background(), map[string]any{}, toolEnv{})

	if result["error"] != "connection reset" {
		t.Errorf("error = %#v, want the transport error surfaced as data, not a panic/Go error", result["error"])
	}
}
