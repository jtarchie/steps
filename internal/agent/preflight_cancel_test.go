package agent

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// A probe that failed because the CALLER walked away learned nothing about
// the target, and must not answer for it later.
//
// probeModelCached stored whatever probeModel returned, and describeProbeError
// renders a cancelled context as `no response within 30s` — indistinguishable
// from a real outage once it is in a cache that is process-wide and
// pipeline-blind. An operator hitting Ctrl-C, a job deadline, or `steps web`
// shutting down mid-probe therefore poisoned a perfectly healthy model for
// the rest of the cache window: an unpinned agent sees probeErr != nil and
// failOver silently reroutes the whole run onto a fallback, or with no
// fallback the job fails saying the model never answered.
//
// reconsiderPin guards the pass that was cancelled. Nothing guarded the entry
// it left behind.

func TestACancelledModelProbeDoesNotCacheItsFailure(t *testing.T) {
	ResetProbeCache()
	t.Setenv("OPENAI_API_KEY", "test")

	url, _ := togglableProbeEndpoint(t)
	ri := config.ResolvedInvocation{
		BaseURL:   url,
		ModelName: "openai/probe",
		APIKeyEnv: "OPENAI_API_KEY",
	}
	settings := &config.Preflight{}

	aborted, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := probeModelCached(aborted, ri, settings)
	if err == nil {
		t.Fatal("a probe on an already-cancelled context reported success")
	}

	_, err = probeModelCached(context.Background(), ri, settings)
	if err != nil {
		t.Fatalf("a serving model failed preflight on a verdict cached from an aborted build: %v", err)
	}
}

func TestACancelledServerProbeDoesNotCacheItsFailure(t *testing.T) {
	ResetProbeCache()

	mcp := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{mcp.server()}}
	spec := config.ToolSpec{MCP: "test", MCPTool: "search_issues"}
	settings := &config.Preflight{}

	aborted, cancel := context.WithCancel(context.Background())
	cancel()

	err := probeServerCached(aborted, cfg, spec, settings)
	if err == nil {
		t.Fatal("a probe on an already-cancelled context reported success")
	}

	err = probeServerCached(context.Background(), cfg, spec, settings)
	if err != nil {
		t.Fatalf("a live MCP server failed preflight on a verdict cached from an aborted build: %v", err)
	}
}

// TestARealProbeFailureIsStillCached is the guard that the fix above bought
// its correctness with a predicate and not by gutting the cache. A model that
// genuinely refused is a fact about the target, and still answers for the
// whole window — which is what keeps a watcher from paying a round trip per
// poll against something it already knows is down.
func TestARealProbeFailureIsStillCached(t *testing.T) {
	ResetProbeCache()
	t.Setenv("OPENAI_API_KEY", "test")

	url, up := togglableProbeEndpoint(t)
	ri := config.ResolvedInvocation{
		BaseURL:   url,
		ModelName: "openai/probe",
		APIKeyEnv: "OPENAI_API_KEY",
	}
	settings := &config.Preflight{}

	up.Store(false)

	_, err := probeModelCached(context.Background(), ri, settings)
	if err == nil {
		t.Fatal("probing a model that answered 503 reported success")
	}

	up.Store(true)

	_, err = probeModelCached(context.Background(), ri, settings)
	if err == nil {
		t.Fatal("a real failure was re-probed instead of read from the cache — the window is what keeps a watcher off a dead endpoint")
	}
}
