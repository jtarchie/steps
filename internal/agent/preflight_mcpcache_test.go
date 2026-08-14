package agent

import (
	"context"
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
