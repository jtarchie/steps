package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpCLIFixtureServer builds an httptest.Server exposing one no-op "ping"
// tool, enough to exercise `steps mcp tools` end to end without a real
// external MCP server.
func mcpCLIFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	srv.AddTool(&sdkmcp.Tool{Name: "ping", Description: "Replies pong.", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "pong"}}}, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

func mcpCLIPipeline(t *testing.T, endpoint string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	yaml := `
mcp_servers:
- name: test
  endpoint: ` + endpoint + `
jobs:
- name: build
  plan: [{ task: noop, run: "true", inputs: [] }]
`

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func TestMCPToolsListsRealServer(t *testing.T) {
	t.Parallel()

	ts := mcpCLIFixtureServer(t)
	path := mcpCLIPipeline(t, ts.URL)

	err := run([]string{"mcp", "tools", path, "test"})
	if err != nil {
		t.Fatalf("run(mcp tools): %v", err)
	}
}

func TestMCPToolsUnknownServer(t *testing.T) {
	t.Parallel()

	ts := mcpCLIFixtureServer(t)
	path := mcpCLIPipeline(t, ts.URL)

	err := run([]string{"mcp", "tools", path, "ghost"})
	if err == nil {
		t.Fatal("run(mcp tools ghost): expected an error for an unconfigured server")
	}
}

func TestMCPLoginRejectsNonOAuthServer(t *testing.T) {
	t.Parallel()

	ts := mcpCLIFixtureServer(t)
	path := mcpCLIPipeline(t, ts.URL) // auth: omitted -> "none", not oauth

	err := run([]string{"mcp", "login", path, "test"})
	if err == nil {
		t.Fatal("run(mcp login): expected an error for a non-oauth server")
	}
}
