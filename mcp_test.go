package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
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

// mcpListPipeline writes a pipeline whose mcp_servers: cover both transports
// and every consumer kind — an agent tool grant, a fix:'s own grant, and a
// resource type's mcp: backend — which is what the USED BY column is made of.
//
// The stdio entries deliberately name a binary no machine has: the ✗ row is
// the outcome under test, and `gopls` (the obvious stand-in) is installed on
// most Go development machines, which would make the assertion pass or fail
// depending on whose laptop it runs on — and spawn a real language server.
func mcpListPipeline(t *testing.T, endpoint string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	yaml := `
mcp_servers:
- name: test
  endpoint: ` + endpoint + `
- name: local
  command: steps-no-such-mcp-server
  args: [mcp]
  cwd: /tmp
- name: perstep
  command: steps-no-such-mcp-server
  cwd: repo

resource_types:
- name: pings
  config:
    mcp:
      server: local
      check: { tool: ping }

agents:
- name: caller
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: test
    tool: ping
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }

jobs:
- name: build
  plan:
  - task: noop
    run: "true"
    inputs: []
    fix:
      agent: fixer
      tools: [{ mcp: perstep }]
`

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestMCPListProbesEveryServer(t *testing.T) {
	ts := mcpCLIFixtureServer(t)
	path := mcpListPipeline(t, ts.URL)

	var err error

	out := captureStdout(t, func() {
		err = run([]string{"mcp", "list", path})
	})

	if err != nil {
		t.Fatalf("run(mcp list): %v", err)
	}

	// The reachable server reports what it exposes; the stdio one reports why
	// it did not answer — and neither outcome fails the command, a report.
	for _, want := range []string{
		"NAME", "TRANSPORT", "TARGET", "AUTH", "USED BY", "STATUS",
		"test", "http", ts.URL, "none", "agent caller", "✓ 1 tool",
		"local", "stdio", "steps-no-such-mcp-server mcp", "cwd: /tmp", "resource_type pings", "✗",
		// A fix:'s own tools: is a grant like any other — the server it names
		// is reachable by a step, so it is not "(unused)".
		"perstep", "job build fix",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mcp list output is missing %q:\n%s", want, out)
		}
	}
}

// A relative cwd: is resolved per agent step, against a build workspace that
// only exists during a run — so probing one from the shell's own directory
// tests nothing and reports a failure that is not real.
func TestMCPListSkipsRelativeCwdServers(t *testing.T) {
	ts := mcpCLIFixtureServer(t)
	path := mcpListPipeline(t, ts.URL)

	var (
		err error
		out string
	)

	out = captureStdout(t, func() {
		err = run([]string{"mcp", "list", path})
	})

	if err != nil {
		t.Fatalf("run(mcp list): %v", err)
	}

	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "perstep") {
			continue
		}

		if strings.Contains(line, "✗") || !strings.Contains(line, "not probed") {
			t.Errorf("the relative-cwd server was probed rather than skipped:\n%s", line)
		}

		return
	}

	t.Errorf("mcp list did not list the relative-cwd server:\n%s", out)
}

func TestMCPProbeReportsCancellation(t *testing.T) {
	t.Parallel()

	cfg, err := config.LoadConfig(mcpListPipeline(t, "http://127.0.0.1:1/mcp"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Interrupting a probe run must not read as "every server is broken":
	// the rows say ✗ context canceled, so the exit status has to say abort.
	_, err = probeMCPServers(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("probeMCPServers on a canceled context returned %v, want context.Canceled", err)
	}
}

func TestMCPListOfflineSkipsTheProbe(t *testing.T) {
	// A port nothing is listening on: --offline must not care, and must not
	// spend the probe timeout finding out.
	path := mcpListPipeline(t, "http://127.0.0.1:1/mcp")

	var err error

	out := captureStdout(t, func() {
		err = run([]string{"mcp", "list", path, "--offline"})
	})

	if err != nil {
		t.Fatalf("run(mcp list --offline): %v", err)
	}

	if !strings.Contains(out, "test") || !strings.Contains(out, "local") {
		t.Errorf("mcp list --offline did not list both servers:\n%s", out)
	}

	if strings.Contains(out, "STATUS") {
		t.Errorf("mcp list --offline reported a STATUS column it never checked:\n%s", out)
	}
}

func TestMCPListUnreachableServerIsAStatusNotAFailure(t *testing.T) {
	path := mcpListPipeline(t, "http://127.0.0.1:1/mcp")

	var err error

	out := captureStdout(t, func() {
		err = run([]string{"mcp", "list", path})
	})

	if err != nil {
		t.Fatalf("run(mcp list) on an unreachable server: %v", err)
	}

	if !strings.Contains(out, "✗") {
		t.Errorf("mcp list did not mark the unreachable server as failed:\n%s", out)
	}
}

func TestMCPListWithNoServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(`
jobs:
- name: build
  plan: [{ task: noop, run: "true", inputs: [] }]
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		err = run([]string{"mcp", "list", path})
	})

	if err != nil {
		t.Fatalf("run(mcp list): %v", err)
	}

	if !strings.Contains(out, "no mcp_servers") {
		t.Errorf("mcp list said nothing about a pipeline with no servers:\n%s", out)
	}
}

func TestMCPListCellRendering(t *testing.T) {
	t.Parallel()

	stdio := config.MCPServer{Name: "gopls", Command: "gopls", Args: []string{"mcp"}, Cwd: "repo"}
	if got, want := mcpTarget(stdio), "gopls mcp (cwd: repo)"; got != want {
		t.Errorf("mcpTarget(stdio) = %q, want %q", got, want)
	}

	if got, want := mcpTarget(config.MCPServer{Endpoint: "https://x/mcp"}), "https://x/mcp"; got != want {
		t.Errorf("mcpTarget(http) = %q, want %q", got, want)
	}

	// The env var NAME is what an operator checks; the value is never read here.
	bearer := config.MCPServer{Auth: config.MCPServerAuth{Type: "bearer", APIKeyEnv: "GITHUB_PAT"}}
	if got, want := mcpAuth(bearer), "bearer $GITHUB_PAT"; got != want {
		t.Errorf("mcpAuth(bearer) = %q, want %q", got, want)
	}

	if got, want := mcpAuth(config.MCPServer{}), "none"; got != want {
		t.Errorf("mcpAuth(unset) = %q, want %q", got, want)
	}

	// The name is the row's first column, so the error's copies of it go.
	reason := mcpStatusReason("linear", errors.New(`mcp server "linear" is not authorized (run `+"`steps mcp login`"+`)`))
	if want := "is not authorized (run `steps mcp login`)"; reason != want {
		t.Errorf("mcpStatusReason = %q, want %q", reason, want)
	}

	if got := mcpStatusReason("test", errors.New(`mcp: connect to "test": dial tcp: refused`)); got != "dial tcp: refused" {
		t.Errorf("mcpStatusReason(connect) = %q", got)
	}

	// How it went is the last few words of a dial error, so a reason too long
	// for the column loses its middle, never its tail.
	long := mcpStatusReason("test", errors.New(
		`mcp: connect to "test": Post "https://api.githubcopilot.com/mcp/": dial tcp 140.82.113.22:443: connect: connection refused`))
	if !strings.HasSuffix(long, "connection refused") {
		t.Errorf("mcpStatusReason dropped the outcome of a long error: %q", long)
	}

	if !strings.HasPrefix(long, `Post "https://api`) {
		t.Errorf("mcpStatusReason dropped what was attempted: %q", long)
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
