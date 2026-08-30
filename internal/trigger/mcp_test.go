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

	"github.com/jtarchie/steps/internal/pipeline"
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

	err = Poll(context.Background(), cfg, st, time.Minute)
	if err == nil {
		t.Fatal("Poll: want an immediate error, not a poll loop logging the same failure forever")
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

// TestWatchStartsDespiteATransientOutage is the other half of the rule the
// test above pins. A tool the server does not expose is fatal: no interval
// grows one. A server that does not answer is NOT — a watcher that quits
// because a VPN was not up yet, or because a token needed the refresh the
// next poll would have done, is found dead on Monday having recovered from
// nothing.
func TestWatchStartsDespiteATransientOutage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A port nothing is listening on: the shape of every outage worth
	// surviving — unreachable now, fine in a minute.
	cfg := loadConfig(t, dir, mcpTriggerPipeline("http://127.0.0.1:1/mcp"))
	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	// Cancelled immediately: Watch must get PAST preflight and into its loop,
	// where the cancellation stops it. A preflight exit would return the
	// unreachable-server error instead.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Watch gets past preflight and then stops on the cancelled context, so
	// the assertion is about WHICH error comes back: anything but the
	// preflight refusal means the unreachable server did not stop it.
	err = Poll(ctx, cfg, st, time.Minute)
	if err != nil && strings.Contains(err.Error(), "preflight") {
		t.Fatalf("Poll: %v; a transient outage must not stop the watcher starting", err)
	}
}

// agentMCPPipeline is a watchable pipeline whose MCP server is reached by an
// AGENT rather than by a resource — the shape that used to slip past watch's
// startup preflight entirely. The agent is granted a tool the server does
// not expose, which no interval will grow.
//
// The model endpoint points at a closed port on purpose: a model that cannot
// answer is a transient problem, so it must NOT be what stops the watcher.
// That is what makes this test about the MCP grant and not about the model.
func agentMCPPipeline(endpoint string) string {
	return fmt.Sprintf(`
defaults:
  preflight:
    timeout: 5s
mcp_servers:
- name: test
  endpoint: %s
resource_types:
- name: shell-dummy
  config:
    check: 'echo ''[{"ref":"a"}]'''
resources:
- name: thing
  type: shell-dummy
  source: {}
agents:
- name: helper
  source:
    model: openai/gpt-4o-mini
    endpoint: http://127.0.0.1:1/v1/
    api_key_env: FAKE_KEY_FOR_TEST
  tools:
  - mcp: test
    tool: no_such_tool
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
  - agent: helper
    messages:
      - hello
`, endpoint)
}

// TestWatchPreflightsAgentMCPServers is the gap this closes: watch's startup
// preflight only ever probed TRIGGER RESOURCES, so a pipeline whose agent
// depended on an unusable MCP server started clean, polled clean, and failed
// at the first real trigger — inside the job, after its gets had already
// run. That is both the least useful moment to learn it and the one where
// the failure reads as being about the trigger rather than the agent.
func TestWatchPreflightsAgentMCPServers(t *testing.T) {
	// Not t.Parallel(): the preflight caches are process-wide, and this test
	// is about what preflight concludes.
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	ts := mcpSearchServer(t)
	cfg := loadConfig(t, dir, agentMCPPipeline(ts.URL))
	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	// Bounded, because the failure mode of a regression here is the poll loop
	// STARTING — and a started poller polls until the package's 10-minute
	// timeout, which reports a hang rather than the assertion that actually
	// failed. With a deadline it comes back in seconds and says so.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = Poll(ctx, cfg, st, time.Minute)
	if err == nil {
		t.Fatal("Poll: started despite an agent MCP grant naming a tool the server does not expose")
	}

	if !strings.Contains(err.Error(), "no_such_tool") {
		t.Errorf("error = %v, want it to name the tool the agent was granted", err)
	}
}

// TestPreflightMarksAModelOutageWaitable is the boundary the test above
// leans on. A watcher is a daemon, and quitting because a model was not
// answering at startup is how one that should have recovered on its own is
// found dead on Monday — so the same probe failure that refuses a `steps
// run` has to leave a watcher polling.
//
// Asserted on the problem rather than through Watch: "the watcher did NOT
// exit" is only observable by letting the poll loop actually run a job,
// which would make this a test of everything except the flag it is about.
func TestPreflightMarksAModelOutageWaitable(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv, and the preflight caches are
	// process-wide. The key has to be SET for this test to be about what it
	// claims — an unset api_key_env is terminal, and correctly so, since no
	// amount of polling exports a variable.
	t.Setenv("FAKE_KEY_FOR_TEST", "not-a-real-key")
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	ts := mcpSearchServer(t)
	cfg := loadConfig(t, dir, strings.Replace(
		agentMCPPipeline(ts.URL), "tool: no_such_tool", "tool: search_versions", 1))

	problems := pipeline.PreflightPipeline(context.Background(), cfg, Resources(cfg))
	if len(problems) == 0 {
		t.Fatal("PreflightPipeline: expected the unreachable model to be reported")
	}

	for _, problem := range problems {
		if !problem.Transient {
			t.Errorf("problem %q is terminal and would stop the watcher: %s", problem.Target, problem.Detail)
		}
	}
}
