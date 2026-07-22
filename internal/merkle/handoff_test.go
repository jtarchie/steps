package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestHandoffHashStabilityWhenUnset proves value-gating: an agent step with
// no handoff: hashes byte-identically to before this feature existed — no
// "handoff" key leaks into its content map.
func TestHandoffHashStabilityWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")
	step := config.Step{Agent: "reviewer", Prompt: "do it"}

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	content, err := AgentContentMap(cfg, step, ri)
	if err != nil {
		t.Fatalf("AgentContentMap: %v", err)
	}

	if _, present := content["handoff"]; present {
		t.Error("unset handoff must not appear in the hashed content")
	}
}

// TestHandoffHashBustsWhenSet proves setting handoff: changes an agent
// step's hash relative to leaving it unset, and that context-vs-tool
// variants differ from each other.
func TestHandoffHashBustsWhenSet(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")

	unset := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it"})
	withContext := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", Handoff: &config.HandoffSpec{Context: true}})
	withTool := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", Handoff: &config.HandoffSpec{Tool: true}})
	withBoth := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", Handoff: &config.HandoffSpec{Context: true, Tool: true}})

	if unset == withContext {
		t.Error("enabling handoff.context should change the hash")
	}

	if unset == withTool {
		t.Error("enabling handoff.tool should change the hash")
	}

	if withContext == withTool {
		t.Error("context-only and tool-only should hash differently")
	}

	if withBoth == withContext || withBoth == withTool {
		t.Error("enabling both should hash differently from either alone")
	}
}
