package llmclient

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestAgentHTTPClientScopesBySessionAgentName guards the field mapping every
// agent LLM depends on: the session is scoped by the agents: entry's name, not
// by the model. AgentName and ModelName sit next to each other on
// ResolvedInvocation, so a swap compiles cleanly and would silently put two
// different agents that share a model back onto one session — the cross-agent
// pinning this scoping exists to prevent.
func TestAgentHTTPClientScopesBySessionAgentName(t *testing.T) {
	t.Parallel()

	client := agentHTTPClient(config.ResolvedInvocation{
		BaseURL:   "https://openrouter.ai/api/v1/",
		ModelName: "anthropic/claude-3.5-sonnet",
		AgentName: "reviewer",
	})
	if client == nil {
		t.Fatal("got nil, want a caching client for an openrouter base url")
	}

	transport, ok := client.Transport.(*openRouterTransport)
	if !ok {
		t.Fatalf("transport = %T, want *openRouterTransport", client.Transport)
	}

	if transport.agent != "reviewer" {
		t.Errorf("transport.agent = %q, want the AgentName %q (not the model)", transport.agent, "reviewer")
	}
}

// TestAgentHTTPClientSkipsNonOpenRouter guards the layering that is NOT
// installed: a non-OpenRouter provider gets the every-provider repair
// transport with no openRouterTransport on top — the session/cache levers
// stay OpenRouter-only even though repair is universal.
func TestAgentHTTPClientSkipsNonOpenRouter(t *testing.T) {
	t.Parallel()

	client := agentHTTPClient(config.ResolvedInvocation{
		BaseURL:   "https://api.openai.com/v1/",
		ModelName: "gpt-4o",
		AgentName: "reviewer",
	})

	if _, ok := client.Transport.(*openRouterTransport); ok {
		t.Errorf("transport = %T, want no openRouterTransport for a non-OpenRouter base url", client.Transport)
	}

	if _, ok := client.Transport.(*repairTransport); !ok {
		t.Errorf("transport = %T, want *repairTransport for a non-OpenRouter base url", client.Transport)
	}
}
