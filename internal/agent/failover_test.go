package agent

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestResolveWithFailoverKeepsThePrimaryForHashing is the mechanism behind the
// not-hashed rule: a step hashes against the invocation as CONFIGURED and runs
// against the one that is actually reachable. If those were the same value, an
// outage would move every agent step's cache key at exactly the moment things
// are already going badly.
func TestResolveWithFailoverKeepsThePrimaryForHashing(t *testing.T) {
	ResetProbeCache()

	cfg := &config.Config{Agents: []config.Agent{{
		Name:   "writer",
		System: "you are a writer",
		Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "PRIMARY_KEY"},
		Fallback: []config.AgentFallback{{
			Source: config.AgentSource{Model: "anthropic/claude-sonnet-4-5", APIKeyEnv: "BACKUP_KEY"},
		}},
	}}}

	step := config.Step{Agent: "writer", Prompt: "write it"}

	// Before any failover, the two are the same invocation.
	primary, effective, err := resolveWithFailover(cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover: %v", err)
	}

	if effective.ModelName != primary.ModelName {
		t.Fatalf("with no failover selected, effective = %q, want the primary %q", effective.ModelName, primary.ModelName)
	}

	// Preflight selects the fallback.
	selectSource("writer", cfg.Agents[0].Fallback[0].Source)

	primary, effective, err = resolveWithFailover(cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover after failover: %v", err)
	}

	if primary.ModelName != "gpt-4o" {
		t.Errorf("primary model = %q, want the CONFIGURED model — this is what the step hashes as", primary.ModelName)
	}

	if effective.ModelName != "claude-sonnet-4-5" {
		t.Errorf("effective model = %q, want the fallback — this is what actually serves the run", effective.ModelName)
	}

	if effective.APIKeyEnv != "BACKUP_KEY" {
		t.Errorf("effective api_key_env = %q, want the fallback's own credential", effective.APIKeyEnv)
	}

	// An outage changes where requests GO, never what the agent IS.
	if effective.Persona != primary.Persona || effective.MaxTurns != primary.MaxTurns {
		t.Error("failover changed the agent's persona or limits, not just its source")
	}

	// The compaction budget follows the model that will actually serve the
	// conversation: a 200K fallback must not inherit a larger primary's.
	if effective.ContextWindow != 200_000 {
		t.Errorf("effective context window = %d, want the fallback model's 200000", effective.ContextWindow)
	}
}
