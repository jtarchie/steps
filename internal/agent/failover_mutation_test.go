package agent

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestNextHostedFallbackSkipsAnUnresolvableEntry(t *testing.T) {
	t.Parallel()

	agent := &config.Agent{
		Name: "writer",
		Fallback: []config.AgentFallback{
			{Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "K1"}},
			{Source: config.AgentSource{Model: "no-provider-no-endpoint"}},
			{Source: config.AgentSource{Model: "anthropic/claude-sonnet-4-5", APIKeyEnv: "K2"}},
		},
	}

	next, nextIndex, ok := nextHostedFallback(agent, config.ResolvedInvocation{}, 0)
	if !ok || nextIndex != 2 || next.ModelName != "claude-sonnet-4-5" {
		t.Errorf("nextHostedFallback(0) = (%q, %d, %v); want index 2, past the entry that does not resolve", next.ModelName, nextIndex, ok)
	}
}
