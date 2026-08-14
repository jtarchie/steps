package trigger

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/workspace"
)

// mcpTriggerServer builds an httptest.Server exposing one "list_versions"
// tool that returns *versions as StructuredContent on every call — a
// pointer so a test can mutate the slice between polls, mirroring how
// writeVersions rewrites the shell-backed dummyPipeline fixture's file.
func mcpTriggerServer(t *testing.T, versions *[]map[string]any) *httptest.Server {
	t.Helper()

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	srv.AddTool(&sdkmcp.Tool{Name: "list_versions", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{StructuredContent: *versions}, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

// mcpTriggerPipeline is the mcp-backed analogue of dummyPipeline: one
// trigger:true get step whose resource type's check: is an mcp tool call
// instead of a shell command.
func mcpTriggerPipeline(endpoint string) string {
	return fmt.Sprintf(`
mcp_servers:
- name: test
  endpoint: %s
resource_types:
- name: mcp-dummy
  config:
    mcp: { server: test, check: { tool: list_versions } }
resources:
- name: thing
  type: mcp-dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
`, endpoint)
}

// TestWatchRefusesToStartOnAnUnsatisfiableCheck is the whole reason watch
// preflights: before this, a trigger resource whose check tool can never
// succeed produced one ERR line per interval, forever — no job enqueued, no
// non-zero exit, nothing to notice. The tool below requires a `query` the
// resource's source: does not have, which no amount of retrying will fix.
func TestWatchRefusesToStartOnAnUnsatisfiableCheck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ts := mcpSearchServer(t)
	cfg := loadConfig(t, dir, strings.Replace(
		mcpTriggerPipeline(ts.URL), "tool: list_versions", "tool: search_versions", 1))
	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	err = Watch(context.Background(), cfg, provider, st, nil, time.Minute, 1, false, "")
	if err == nil {
		t.Fatal("Watch: want an immediate error, not a poll loop logging the same failure forever")
	}

	if !strings.Contains(err.Error(), "query") {
		t.Errorf("error = %v, want it to name the argument the tool requires", err)
	}
}

// mcpSearchServer exposes one tool that DECLARES a required argument, like
// every real server's tools do.
func mcpSearchServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)

	srv.AddTool(&sdkmcp.Tool{
		Name: "search_versions",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []string{"query"},
		},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{StructuredContent: []map[string]any{{"id": "1"}}}, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

// TestPollOnceMCPBackedResource proves the claim documented on
// resource.CheckVersions's mcp branch and on checkResource/pollOnce
// themselves: the trigger poll/diff/enqueue loop needs zero changes to work
// against an mcp-backed resource type, since it only depends on
// CheckVersions's already-generic []map[string]any return contract. Mirrors
// TestPollOnceEnqueuesOnVersionChange's shell-backed sequence exactly
// (baseline seed -> unchanged poll enqueues nothing -> changed poll
// enqueues the job), against a real MCP server instead of a versions file.
func TestPollOnceMCPBackedResource(t *testing.T) {
	t.Parallel()

	versions := make([]map[string]any, 0, 2)
	versions = append(versions, map[string]any{"id": "1"})

	dir := t.TempDir()
	ts := mcpTriggerServer(t, &versions)
	cfg := loadConfig(t, dir, mcpTriggerPipeline(ts.URL))
	st := mustOpenStore(t, dir)

	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (unchanged): %v", err)
	}

	if len(enqueued) != 0 {
		t.Errorf("unchanged poll enqueued %v, want none", enqueued)
	}

	versions = append(versions, map[string]any{"id": "2"})

	enqueued, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (changed): %v", err)
	}

	if !reflect.DeepEqual(enqueued, []string{"build"}) {
		t.Fatalf("changed poll enqueued %v, want [build]", enqueued)
	}
}
