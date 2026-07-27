package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestToolCallGuardHashStabilityWhenUnset proves value-gating: a custom tool
// with neither max_calls: nor args: hashes byte-identically to before this
// feature existed — no "max_calls"/"args" keys leak into its content map.
func TestToolCallGuardHashStabilityWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review"}}, "")
	step := config.Step{Agent: "reviewer", Prompt: "do it"}

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

	if _, present := tools[0]["max_calls"]; present {
		t.Error("unset max_calls must not appear in the hashed content")
	}

	if _, present := tools[0]["args"]; present {
		t.Error("unset args must not appear in the hashed content")
	}
}

// TestToolCallGuardHashBustsWhenSet proves setting max_calls: or args: on a
// custom tool changes the step's hash relative to leaving them unset, and
// that changing either value changes the hash again.
func TestToolCallGuardHashBustsWhenSet(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}

	unset := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review"}}, ""), step)
	withMaxCalls := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review", MaxCalls: 1}}, ""), step)
	withMaxCallsChanged := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review", MaxCalls: 2}}, ""), step)
	withArgs := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review", Args: map[string]string{"repo": "a"}}}, ""), step)
	withArgsChanged := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review", Args: map[string]string{"repo": "b"}}}, ""), step)

	if unset == withMaxCalls {
		t.Error("setting max_calls should change the hash")
	}

	if withMaxCalls == withMaxCallsChanged {
		t.Error("changing max_calls should change the hash")
	}

	if unset == withArgs {
		t.Error("setting args should change the hash")
	}

	if withArgs == withArgsChanged {
		t.Error("changing an args value should change the hash")
	}
}

// TestMaxOutputBytesHashStabilityWhenUnset proves value-gating: a tool that
// sets no max_output_bytes: hashes byte-identically to before the field
// existed, so adding it invalidates no existing pipeline's cache.
func TestMaxOutputBytesHashStabilityWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review"}}, "")
	step := config.Step{Agent: "reviewer", Prompt: "do it"}

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

	if _, present := tools[0]["max_output_bytes"]; present {
		t.Error("unset max_output_bytes must not appear in the hashed content")
	}
}

// TestMaxOutputBytesHashIsPinned is the byte-identity guarantee stated as a
// literal rather than a recompute: this hash was captured before
// max_output_bytes: existed. If it changes, some field started leaking into
// the content map unconditionally and every cached agent step in every
// existing pipeline just got invalidated.
func TestMaxOutputBytesHashIsPinned(t *testing.T) {
	t.Parallel()

	// Captured by running this fixture against the tree as it stood before
	// max_output_bytes: was added, in a worktree at that commit.
	const wantHash = "480a4f64eb4deca82307ddde0d04db3a9749549bd7fbe460f80e3bb1ee3d2649"

	got := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review"}}, ""), config.Step{Agent: "reviewer", Prompt: "do it"})
	if got != wantHash {
		t.Errorf("agent step hash = %q, want the pinned %q", got, wantHash)
	}
}

// TestMaxOutputBytesHashBustsWhenSet proves the field is hashed once set:
// it changes what a call returns to the model, so it changes the
// conversation the step produces.
func TestMaxOutputBytesHashBustsWhenSet(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}

	unset := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review"}}, ""), step)
	set := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review", MaxOutputBytes: 4000}}, ""), step)
	changed := mustAgentHash(t, agentCfg([]config.ToolSpec{{Name: "post_review", Run: "gh pr review", MaxOutputBytes: 8000}}, ""), step)

	if unset == set {
		t.Error("setting max_output_bytes should change the hash")
	}

	if set == changed {
		t.Error("changing max_output_bytes should change the hash")
	}
}
