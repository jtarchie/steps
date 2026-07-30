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

// TestHandoffNoteHashStabilityWhenUnset proves the same value-gating for
// handoff_note: a step that neither sends nor receives one keeps the content
// map it had before the feature existed.
func TestHandoffNoteHashStabilityWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")

	content, err := AgentContentMap(cfg, config.Step{Agent: "reviewer", Prompt: "do it"}, mustRI(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it"}))
	if err != nil {
		t.Fatalf("AgentContentMap: %v", err)
	}

	for _, key := range []string{"handoff_note", "handoff_note_from"} {
		if _, present := content[key]; present {
			t.Errorf("unset handoff_note must not put %q in the hashed content", key)
		}
	}
}

// TestHandoffNoteHashBustsWhenSet proves both sides are identity: sending a
// note adds a required tool to what the step executes with, and receiving one
// adds an injected context block, so neither may share a hash with a step
// that does neither.
func TestHandoffNoteHashBustsWhenSet(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")

	unset := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it"})
	sending := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", HandoffNote: true})
	receiving := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", HandoffNoteFrom: "planner"})
	fromOther := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", HandoffNoteFrom: "coder"})

	if unset == sending {
		t.Error("declaring handoff_note should change the hash")
	}

	if unset == receiving {
		t.Error("receiving a handoff note should change the hash")
	}

	if receiving == fromOther {
		t.Error("receiving from a different sender should hash differently")
	}
}

// mustRI resolves step's invocation or fails the test.
func mustRI(t *testing.T, cfg *config.Config, step config.Step) config.ResolvedInvocation {
	t.Helper()

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	return ri
}
