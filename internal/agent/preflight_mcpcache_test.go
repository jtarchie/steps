package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestPreflightDoesNotShareOneToolsVerdictAcrossGrants covers a hole in the
// probe cache that defeated the check it was caching.
//
// probeServer asks two questions: does the server answer, and does it expose
// the tools THIS grant names. The first answer is shared across grants; the
// second is not. Keyed without MCPTool, `{mcp: test, tool: search_issues}`
// and `{mcp: test, tool: no_such_tool}` collided, so whichever was probed
// first answered for both — and a grant naming a tool the server does not
// have passed preflight because a DIFFERENT grant's tool existed.
//
// It only reproduces in a process that has already probed the server, which
// is every `steps watch` and every second job in one — the cases preflight
// matters most in. It was found by a watch test that started cleanly and
// then polled forever against a pipeline that could never run.
func TestPreflightDoesNotShareOneToolsVerdictAcrossGrants(t *testing.T) {
	ResetProbeCache()

	mcp := newCountingMCPServer(t)

	cfg := &config.Config{
		MCPServers: []config.MCPServer{mcp.server()},
		Agents: []config.Agent{
			{
				Name:   "good",
				Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "KEY"},
				Tools:  []config.ToolSpec{{MCP: "test", MCPTool: "search_issues"}},
			},
			{
				Name:   "bad",
				Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "KEY"},
				Tools:  []config.ToolSpec{{MCP: "test", MCPTool: "no_such_tool"}},
			},
		},
	}

	settings := &config.Preflight{}

	// The valid grant first, so its verdict is the one sitting in the cache
	// when the invalid grant is probed — the ordering that made the bug
	// invisible.
	err := probeServerCached(context.Background(), cfg, cfg.Agents[0].Tools[0], settings)
	if err != nil {
		t.Fatalf("probing a grant for a tool the server DOES expose: %v", err)
	}

	err = probeServerCached(context.Background(), cfg, cfg.Agents[1].Tools[0], settings)
	if err == nil {
		t.Fatal("a grant naming a tool the server does not expose passed preflight, on a cached verdict for a different tool")
	}
}

// TestPreflightStillCachesAnIdenticalGrant is the other half: splitting the
// key per tool must not cost the cache its reason to exist. Two probes of
// the SAME grant still make one connection, which is what keeps a
// long-running watcher from paying a round trip per poll.
func TestPreflightStillCachesAnIdenticalGrant(t *testing.T) {
	ResetProbeCache()

	mcp := newCountingMCPServer(t)

	cfg := &config.Config{
		MCPServers: []config.MCPServer{mcp.server()},
	}

	spec := config.ToolSpec{MCP: "test", MCPTool: "search_issues"}
	settings := &config.Preflight{}

	for range 2 {
		err := probeServerCached(context.Background(), cfg, spec, settings)
		if err != nil {
			t.Fatalf("probeServerCached: %v", err)
		}
	}

	if *mcp.listCalls != 1 {
		t.Fatalf("tools/list called %d times for the same grant, want 1", *mcp.listCalls)
	}
}

// TestPreflightDoesNotShareOneServersVerdictAcrossPipelines covers the same
// unscoped-cache class one level up from the grant.
//
// The key named the server by NAME alone, and nothing about the definition
// behind it. Under `steps web app.yml infra.yml` two pipelines may each
// declare `mcp_servers: [{name: test, ...}]` pointing at entirely different
// things, and one answer served both: a healthy server vouched for a broken
// neighbour, and a broken one condemned a healthy neighbour.
//
// internal/resource/preflight.go was already immune because it keys on the
// definition, which is the stronger fix — it distinguishes two pipelines AND
// two revisions of the same pipeline.
func TestPreflightDoesNotShareOneServersVerdictAcrossPipelines(t *testing.T) {
	ResetProbeCache()

	mcp := newCountingMCPServer(t)
	spec := config.ToolSpec{MCP: "test", MCPTool: "search_issues"}
	settings := &config.Preflight{}

	live := &config.Config{MCPServers: []config.MCPServer{mcp.server()}}
	dead := &config.Config{MCPServers: []config.MCPServer{{
		Name:    "test",
		Command: "/nonexistent/mcp-server",
	}}}

	err := probeServerCached(context.Background(), live, spec, settings)
	if err != nil {
		t.Fatalf("probing the live server: %v", err)
	}

	err = probeServerCached(context.Background(), dead, spec, settings)
	if err == nil {
		t.Fatal("a pipeline's unstartable server passed preflight on a neighbour's cached verdict")
	}
}

// TestPreflightProbesEveryGrantOnAServer is the dedupe half of the same bug.
//
// probeAgentServers deduped by server NAME, so the second grant on a server
// never reached the cache at all — and the per-grant key the cache had just
// grown existed but was never computed. Whether preflight caught a bad tool
// name came down to which agent happened to be listed first.
func TestPreflightProbesEveryGrantOnAServer(t *testing.T) {
	ResetProbeCache()

	mcp := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{mcp.server()}}
	settings := &config.Preflight{}

	good := config.ResolvedInvocation{ToolSpecs: []config.ToolSpec{{MCP: "test", MCPTool: "search_issues"}}}
	bad := config.ResolvedInvocation{ToolSpecs: []config.ToolSpec{{MCP: "test", MCPTool: "no_such_tool"}}}

	// The good agent first: the order in which the bug reported nothing.
	seen := map[string]bool{}
	problems := probeAgentServers(context.Background(), cfg, good, settings, seen)
	problems = append(problems, probeAgentServers(context.Background(), cfg, bad, settings, seen)...)

	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1 naming no_such_tool: %+v", len(problems), problems)
	}

	if !strings.Contains(problems[0].Detail, "no_such_tool") {
		t.Errorf("problem = %q, want it to name the missing tool", problems[0].Detail)
	}
}
