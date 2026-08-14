package resource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	// Reads `title` off the TOP level of its arguments, like every real MCP
	// tool reads its own published parameters — the fixture would not be
	// testing the contract if it went looking inside an envelope.
	srv.AddTool(&sdkmcp.Tool{Name: "create_issue", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var args map[string]any

			_ = json.Unmarshal(req.Params.Arguments, &args)

			return &sdkmcp.CallToolResult{
				StructuredContent: map[string]any{"id": "new-1", "title": args["title"], "team": args["team"]},
			}, nil
		})

	// A check tool that reports what it was called with, as a version array,
	// so a test can assert on the arguments a check sends.
	srv.AddTool(&sdkmcp.Tool{Name: "list_issues_echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var args map[string]any

			_ = json.Unmarshal(req.Params.Arguments, &args)

			return &sdkmcp.CallToolResult{StructuredContent: []map[string]any{{"sent": args}}}, nil
		})

	// Two tools that DECLARE a required argument, the way every real server's
	// tools do — what preflight reads to decide whether a call can ever work.
	srv.AddTool(&sdkmcp.Tool{
		Name: "search_issues",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []string{"query"},
		},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{StructuredContent: []map[string]any{{"id": "1"}}}, nil
	})

	srv.AddTool(&sdkmcp.Tool{
		Name: "post_issue",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"message": map[string]any{"type": "string"}},
			"required":   []string{"message"},
		},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{StructuredContent: map[string]any{"id": "new-1"}}, nil
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

	// No args: on the in:, so the arguments are the resource's source:,
	// verbatim — nothing wrapped around it and nothing added to it.
	echoed, ok := result["echoed"].(map[string]any)
	if !ok || echoed["team"] != "ENG" || len(echoed) != 1 {
		t.Errorf("result.json = %+v, want source sent verbatim as the arguments", result)
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

// TestCheckVersionsMCPSendsSourceVerbatim pins the contract that decides
// whether an off-the-shelf MCP server is usable at all: a remote tool's
// arguments are its OWN published schema, so a check with no args: sends the
// source as the argument object and wraps it in nothing. Sending
// {"source": source} instead means a tool requiring `query` never sees one,
// no matter what the pipeline author writes.
func TestCheckVersionsMCPSendsSourceVerbatim(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues_echo")

	versions, err := CheckVersions(context.Background(), cfg, rt, map[string]any{"query": "to:me"})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	sent, ok := versions[0]["sent"].(map[string]any)
	if !ok || sent["query"] != "to:me" {
		t.Fatalf("arguments = %+v, want the source itself", versions[0]["sent"])
	}
}

// TestCheckVersionsMCPArgsTemplate covers the other half: a tool whose
// parameter names differ from the source's own keys is reached by naming the
// mapping, rendered over the same {source} the shell path templates against.
func TestCheckVersionsMCPArgsTemplate(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues_echo")
	rt.Config.MCP.Check.Args = map[string]any{
		"query": "in:{{ .source.channel }} is:thread",
		"limit": 20,
	}

	versions, err := CheckVersions(context.Background(), cfg, rt, map[string]any{"channel": "eng"})
	if err != nil {
		t.Fatalf("CheckVersions: %v", err)
	}

	sent, _ := versions[0]["sent"].(map[string]any)
	if sent["query"] != "in:eng is:thread" {
		t.Errorf("query = %#v, want the rendered template", sent["query"])
	}

	// A non-string leaf is the tool's own type, not a string: `limit: 20`
	// must arrive as a number.
	if sent["limit"] != float64(20) {
		t.Errorf("limit = %#v, want the number 20 passed through untouched", sent["limit"])
	}
}

// TestRunInMCPArgsTemplate is the case in: exists for: the fields the tool
// needs (which thread, which issue) live on the VERSION a check produced, not
// on the source.
func TestRunInMCPArgsTemplate(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.In = &config.MCPToolCall{
		Tool: "get_issue",
		Args: map[string]any{"issue_id": "{{ .version.id }}", "team": "{{ .source.team }}"},
	}
	destDir := t.TempDir()

	err := RunIn(context.Background(), cfg, rt, map[string]any{"team": "ENG"}, map[string]any{"id": "42"}, nil, destDir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
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

	echoed, _ := result["echoed"].(map[string]any)
	if echoed["issue_id"] != "42" || echoed["team"] != "ENG" {
		t.Errorf("arguments = %+v, want the version and source fields the mapping names", echoed)
	}
}

// TestRunInMCPArgsMissingKeyErrors: a mapping that names a field the version
// does not carry is a typo, and a typo must fail loudly rather than silently
// send nothing — the same rule template.Render enforces for a shell check.
func TestRunInMCPArgsMissingKeyErrors(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.In = &config.MCPToolCall{
		Tool: "get_issue",
		Args: map[string]any{"issue_id": "{{ .version.nope }}"},
	}

	err := RunIn(context.Background(), cfg, rt, map[string]any{}, map[string]any{"id": "42"}, nil, t.TempDir())
	if err == nil {
		t.Fatal("RunIn: want an error naming the unresolvable args template")
	}

	if !strings.Contains(err.Error(), "args") {
		t.Errorf("error = %v, want it to name args", err)
	}
}

// TestRunOutMCPArgsTemplate: the put's params: are the payload, and args: is
// how they reach a tool whose parameter names differ (Slack's send tool takes
// `message`, not `text`).
func TestRunOutMCPArgsTemplate(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.Out = &config.MCPToolCall{
		Tool: "create_issue",
		Args: map[string]any{"title": "{{ .params.text }}", "team": "{{ .source.team }}"},
	}

	result, err := RunOut(context.Background(), cfg, rt,
		map[string]any{"team": "ENG"}, map[string]any{"text": "Triage needed"}, t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	if result["title"] != "Triage needed" || result["team"] != "ENG" {
		t.Errorf("result = %+v, want the mapping's arguments to have reached the tool", result)
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

// writeParamFile creates dir/rel with the given contents under a fresh
// temp root, returning that root — the stand-in for a put's read view.
func writeParamFile(t *testing.T, rel, contents string) string {
	t.Helper()

	root := t.TempDir()

	full := filepath.Join(root, filepath.FromSlash(rel))

	err := os.MkdirAll(filepath.Dir(full), 0o750)
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err = os.WriteFile(full, []byte(contents), 0o600)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	return root
}

// TestRunOutMCPResolvesParamFile is the reason this feature exists: an MCP
// out: has no working directory, so without the marker a put could only
// ever send text the pipeline author typed — never a reply an agent wrote.
func TestRunOutMCPResolvesParamFile(t *testing.T) {
	t.Parallel()

	body := "Deploys are gated on the release job.\n\nSee config/deploy.rb."
	srcDir := writeParamFile(t, "answer/reply.md", body+"\n")

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.Out = &config.MCPToolCall{Tool: "create_issue"}

	result, err := RunOut(context.Background(), cfg, rt,
		map[string]any{"team": "ENG"},
		map[string]any{"title": map[string]any{"file": "answer/reply.md"}},
		srcDir)
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	// Trimmed like load_var:, so a redirect's trailing newline never reaches
	// an API that treats it as part of an id.
	if result["title"] != body {
		t.Errorf("title = %q, want the trimmed file contents %q", result["title"], body)
	}
}

// TestRunOutMCPLeavesMultiKeyObjectAlone is the collision case that decides
// whether spelling the marker INSIDE params is safe: a tool whose parameter
// genuinely is an object carrying a `file` field alongside others must keep
// passing through untouched.
func TestRunOutMCPLeavesMultiKeyObjectAlone(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.Out = &config.MCPToolCall{Tool: "create_issue"}

	result, err := RunOut(context.Background(), cfg, rt,
		map[string]any{},
		map[string]any{"title": map[string]any{"file": "report.pdf", "label": "Q3"}},
		t.TempDir())
	if err != nil {
		t.Fatalf("RunOut: %v", err)
	}

	got, ok := result["title"].(map[string]any)
	if !ok {
		t.Fatalf("title = %#v, want the object passed through unread", result["title"])
	}

	if got["file"] != "report.pdf" || got["label"] != "Q3" {
		t.Errorf("title = %+v, want both keys intact", got)
	}
}

func TestResolveParamFilesNested(t *testing.T) {
	t.Parallel()

	srcDir := writeParamFile(t, "answer/reply.md", "hello")

	resolved, err := resolveParamFiles(map[string]any{
		"blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"file": "answer/reply.md"}},
		},
	}, srcDir)
	if err != nil {
		t.Fatalf("resolveParamFiles: %v", err)
	}

	blocks, _ := resolved["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v", resolved["blocks"])
	}

	block, _ := blocks[0].(map[string]any)
	if block["text"] != "hello" {
		t.Errorf("nested marker not resolved: %+v", block)
	}
}

func TestResolveParamFilesPathRules(t *testing.T) {
	t.Parallel()

	srcDir := writeParamFile(t, "answer/reply.md", "hello")

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "absolute", path: "/etc/passwd", want: "is absolute"},
		{name: "escaping", path: "../../secrets.txt", want: "escapes the workspace"},
		{name: "no artifact", path: "reply.md", want: "names no artifact"},
		{name: "empty", path: "", want: "is empty"},
		{name: "missing file", path: "answer/absent.md", want: "is its artifact in the put's inputs:?"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveParamFiles(map[string]any{"text": map[string]any{"file": test.path}}, srcDir)
			if err == nil {
				t.Fatalf("resolveParamFiles(%q) succeeded, want an error", test.path)
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

// TestRunInMCPArgsKeepNumbersExact: a version comes off the wire through
// encoding/json, so every number in it is a float64 — and text/template
// prints a float64 with %v, which reaches for exponent notation well inside
// the range of ordinary identifiers. Without normalization the documented
// `issue_id: "{{ .version.id }}"` sends "1.23456789e+08" for issue 123456789
// and a Slack message_ts as "1.717171717123456e+09", which no server accepts.
func TestRunInMCPArgsKeepNumbersExact(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)
	rt := mcpResourceType("list_issues")
	rt.Config.MCP.In = &config.MCPToolCall{
		Tool: "get_issue",
		Args: map[string]any{"issue_id": "{{ .version.id }}", "thread_ts": "{{ .version.ts }}"},
	}
	destDir := t.TempDir()

	// Exactly the shape json.Unmarshal produces for a version object.
	var version map[string]any

	err := json.Unmarshal([]byte(`{"id": 123456789, "ts": 1717171717.123456}`), &version)
	if err != nil {
		t.Fatalf("unmarshal version: %v", err)
	}

	err = RunIn(context.Background(), cfg, rt, map[string]any{}, version, nil, destDir)
	if err != nil {
		t.Fatalf("RunIn: %v", err)
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

	echoed, _ := result["echoed"].(map[string]any)
	if echoed["issue_id"] != "123456789" || echoed["thread_ts"] != "1717171717.123456" {
		t.Errorf("arguments = %+v, want the version's digits verbatim, not %%v's exponent form", echoed)
	}
}
