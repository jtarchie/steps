package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// agentCfg builds a Config whose `reviewer` agent grants the tools in
// reviewerTools, plus an `extra` child agent with childSystem as its persona.
func agentCfg(reviewerTools []config.ToolSpec, childSystem string) *config.Config {
	return &config.Config{
		Agents: []config.Agent{
			{
				Name:   "reviewer",
				Source: config.AgentSource{Model: "lmstudio/qwen"},
				Tools:  reviewerTools,
			},
			{
				Name:   "extra",
				Source: config.AgentSource{Model: "lmstudio/qwen"},
				System: childSystem,
			},
		},
	}
}

func mustAgentHash(t *testing.T, cfg *config.Config, step config.Step) string {
	t.Helper()

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	content, err := AgentContentMap(cfg, step, ri)
	if err != nil {
		t.Fatalf("AgentContentMap: %v", err)
	}

	hash, err := HashNode(NodeKindAgent, content, "")
	if err != nil {
		t.Fatalf("HashNode: %v", err)
	}

	return hash
}

// TestSubAgentHashStabilityWithoutSubAgents proves value-gating: an agent that
// grants only builtins/custom tools hashes byte-identically whether or not the
// sub-agent feature exists — the tool content map for a non-sub-agent tool is
// unchanged.
func TestSubAgentHashStabilityWithoutSubAgents(t *testing.T) {
	t.Parallel()

	cfg := agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "")
	step := config.Step{Agent: "reviewer", Messages: []string{"do it"}}

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	content, err := AgentContentMap(cfg, step, ri)
	if err != nil {
		t.Fatalf("AgentContentMap: %v", err)
	}

	tools, ok := content["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools content = %#v, want one entry", content["tools"])
	}

	// The exact map a non-sub-agent tool hashed as before this feature — no
	// "agent"/"invocation" keys leak in.
	want := map[string]any{"builtin": "read_file", "name": "", "description": "", "run": ""}
	for k, v := range want {
		if tools[0][k] != v {
			t.Errorf("tools[0][%q] = %#v, want %#v", k, tools[0][k], v)
		}
	}

	if _, present := tools[0]["invocation"]; present {
		t.Error("non-sub-agent tool content must not carry an invocation key")
	}
}

// TestSubAgentEditBustsParentHash proves that editing the child agent's
// identity (its persona) changes the parent step's hash — the recursive fold.
func TestSubAgentEditBustsParentHash(t *testing.T) {
	t.Parallel()

	tools := []config.ToolSpec{{Agent: "extra", Description: "delegate"}}
	step := config.Step{Agent: "reviewer", Messages: []string{"do it"}}

	before := mustAgentHash(t, agentCfg(tools, "you are v1"), step)
	after := mustAgentHash(t, agentCfg(tools, "you are v2"), step)

	if before == after {
		t.Error("editing the child agent's persona should bust the parent step's hash, but hashes matched")
	}
}

// TestSubAgentGrantChangesHash proves that granting a sub-agent changes the
// hash relative to not granting it (so switching a step into delegation
// correctly invalidates its cache).
func TestSubAgentGrantChangesHash(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Messages: []string{"do it"}}

	withBuiltin := mustAgentHash(t, agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, ""), step)
	withSubAgent := mustAgentHash(t, agentCfg([]config.ToolSpec{{Agent: "extra", Description: "delegate"}}, ""), step)

	if withBuiltin == withSubAgent {
		t.Error("granting a sub-agent tool should change the hash vs a builtin grant")
	}
}
