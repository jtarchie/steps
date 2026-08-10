package resource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
)

// mcpFixtureServer builds an httptest.Server exposing three tools:
// list_issues (returns versions via StructuredContent), search_issues_text
// (returns the same shape via a text content block, for the fallback-parse
// path), and get_issue/create_issue (echo their arguments back so tests can
// assert on exactly what was sent).
func mcpFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	srv.AddTool(&sdkmcp.Tool{Name: "list_issues", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{
				StructuredContent: []map[string]any{{"id": "1"}, {"id": "2"}},
			}, nil
		})

	srv.AddTool(&sdkmcp.Tool{Name: "list_issues_text", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `[{"id":"1"},{"id":"2"}]`}},
			}, nil
		})

	srv.AddTool(&sdkmcp.Tool{Name: "list_issues_error", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{
				IsError: true,
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "upstream rate limited"}},
			}, nil
		})

	// StructuredContent is object-shaped (spec-compliant tool output, per
	// the MCP spec: structured output must marshal to a JSON *object*), not
	// the array parseVersionArray wants — but the SDK also mirrors it into
	// a text block, which IS the array. Regression fixture for the
	// StructuredContent-present-but-wrong-shape fallback.
	srv.AddTool(&sdkmcp.Tool{Name: "list_issues_object_structured", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{
				StructuredContent: map[string]any{"issues": []map[string]any{{"id": "1"}, {"id": "2"}}},
				Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: `[{"id":"1"},{"id":"2"}]`}},
			}, nil
		})

	// No structured content; a human-readable block precedes the JSON
	// block. Regression fixture for scanning every text block, not just
	// the first.
	srv.AddTool(&sdkmcp.Tool{Name: "list_issues_prose_then_json", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{
					&sdkmcp.TextContent{Text: "Found 2 issues:"},
					&sdkmcp.TextContent{Text: `[{"id":"1"},{"id":"2"}]`},
				},
			}, nil
		})

	srv.AddTool(&sdkmcp.Tool{Name: "get_issue", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var args map[string]any

			_ = json.Unmarshal(req.Params.Arguments, &args)

			return &sdkmcp.CallToolResult{
				StructuredContent: map[string]any{"echoed": args},
				Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: "issue body text"}},
			}, nil
		})

	srv.AddTool(&sdkmcp.Tool{Name: "create_issue", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var args map[string]any

			_ = json.Unmarshal(req.Params.Arguments, &args)

			params, _ := args["params"].(map[string]any)

			return &sdkmcp.CallToolResult{
				StructuredContent: map[string]any{"id": "new-1", "title": params["title"]},
			}, nil
		})

	srv.AddTool(&sdkmcp.Tool{Name: "create_issue_unparsable", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "not json"}}}, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

func mcpFixtureConfig(t *testing.T) *config.Config {
	t.Helper()

	ts := mcpFixtureServer(t)

	return &config.Config{MCPServers: []config.MCPServer{{Name: "test", Endpoint: ts.URL}}}
}

func mcpResourceType(checkTool string) config.ResourceType {
	return config.ResourceType{
		Name: "linear-issues",
		Config: config.ResourceTypeConfig{
			MCP: &config.MCPResourceConfig{
				Server: "test",
				Check:  &config.MCPToolCall{Tool: checkTool},
			},
		},
	}
}

func TestCheckVersionsMCPStructuredContent(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")

	versions, err := CheckVersions(context.Background(), cfg, rt, map[string]any{"team": "ENG"})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if len(versions) != 2 || versions[1]["id"] != "2" {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestCheckVersionsMCPTextContentFallback(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues_text")

	versions, err := CheckVersions(context.Background(), cfg, rt, map[string]any{})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if len(versions) != 2 || versions[0]["id"] != "1" {
		t.Fatalf("versions = %+v", versions)
	}
}

// TestCheckVersionsMCPObjectStructuredContentFallsBackToText covers a
// spec-compliant tool whose StructuredContent is object-shaped (not the
// array parseVersionArray wants) but whose mirrored text content IS the
// array — previously this errored outright instead of trying the text
// block, breaking any server (e.g. Linear's) that returns structured
// output this way.
func TestCheckVersionsMCPObjectStructuredContentFallsBackToText(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues_object_structured")

	versions, err := CheckVersions(context.Background(), cfg, rt, map[string]any{})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if len(versions) != 2 || versions[1]["id"] != "2" {
		t.Fatalf("versions = %+v", versions)
	}
}

// TestCheckVersionsMCPScansEveryTextBlock covers a tool that emits a
// human-readable block before the JSON block — previously only the first
// text block was ever tried, so this failed to parse even though a usable
// array was present later in Content.
func TestCheckVersionsMCPScansEveryTextBlock(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues_prose_then_json")

	versions, err := CheckVersions(context.Background(), cfg, rt, map[string]any{})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if len(versions) != 2 || versions[0]["id"] != "1" {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestCheckVersionsMCPToolError(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues_error")

	_, err := CheckVersions(context.Background(), cfg, rt, map[string]any{})
	if err == nil {
		t.Fatal("CheckVersions: expected an error when the mcp tool returns IsError")
	}
}

func TestRunInMCPDefaultsToVersionJSON(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues") // In left nil
	destDir := t.TempDir()

	err := RunIn(context.Background(), cfg, rt, map[string]any{"team": "ENG"}, map[string]any{"id": "1"}, nil, destDir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "version.json")) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("read version.json: %v", err)
	}

	var got map[string]any

	err = json.Unmarshal(data, &got)
	if err != nil {
		t.Fatalf("unmarshal version.json: %v", err)
	}

	if got["id"] != "1" {
		t.Errorf("version.json = %+v, want id=1", got)
	}

	// No in: tool configured, so nothing else should be written.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("destDir entries = %v, want only version.json", entries)
	}
}

func TestRunInMCPMaterializesToolResult(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.In = &config.MCPToolCall{Tool: "get_issue"}
	destDir := t.TempDir()

	err := RunIn(context.Background(), cfg, rt, map[string]any{"team": "ENG"}, map[string]any{"id": "1"}, nil, destDir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
	}

	// version.json is always written, even when in: is also set.
	_, err = os.Stat(filepath.Join(destDir, "version.json"))
	if err != nil {
		t.Errorf("version.json missing: %v", err)
	}

	resultData, err := os.ReadFile(filepath.Join(destDir, "result.json")) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}

	var result map[string]any

	err = json.Unmarshal(resultData, &result)
	if err != nil {
		t.Fatalf("unmarshal result.json: %v", err)
	}

	echoed, ok := result["echoed"].(map[string]any)
	if !ok || echoed["version"] == nil {
		t.Errorf("result.json = %+v, want the echoed {source, version} args", result)
	}

	text, err := os.ReadFile(filepath.Join(destDir, "content-0.txt")) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatalf("read content-0.txt: %v", err)
	}

	if string(text) != "issue body text" {
		t.Errorf("content-0.txt = %q", text)
	}
}

func TestRunOutMCP(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.Out = &config.MCPToolCall{Tool: "create_issue"}

	result, err := RunOut(context.Background(), cfg, rt, map[string]any{"team": "ENG"}, map[string]any{"title": "Triage needed"}, t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if result["id"] != "new-1" || result["title"] != "Triage needed" {
		t.Errorf("result = %+v", result)
	}
}

func TestRunOutMCPUnparsableResultIsNilNotError(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.Out = &config.MCPToolCall{Tool: "create_issue_unparsable"}

	result, err := RunOut(context.Background(), cfg, rt, map[string]any{}, map[string]any{}, t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v, want a nil result instead of an error (mirrors the shell backend's own convention)", err)
	}

	if result != nil {
		t.Errorf("result = %+v, want nil", result)
	}
}

func TestShellBackendUnaffectedByMCPBranch(t *testing.T) {
	t.Parallel()

	// A resource type with no mcp: block must still take the ordinary shell
	// path — proves the rt.Config.MCP != nil branch is truly value-gated.
	rt := config.ResourceType{
		Name:   "dummy",
		Config: config.ResourceTypeConfig{Check: `printf '[{"ref":"1"}]'`},
	}

	versions, err := CheckVersions(context.Background(), nil, rt, map[string]any{})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	if len(versions) != 1 || versions[0]["ref"] != "1" {
		t.Fatalf("versions = %+v", versions)
	}
}
