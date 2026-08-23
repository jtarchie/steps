package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func agentStepWithAssert(assert *config.Assert) config.Step {
	return config.Step{Agent: "reviewer", Messages: []string{"do it"}, Assert: assert}
}

// TestAssertToolCallsHashStabilityWhenUnset proves value-gating: an assert
// without tool_calls hashes byte-identically to before this field existed.
func TestAssertToolCallsHashStabilityWhenUnset(t *testing.T) {
	t.Parallel()

	stdout := "posted"
	cfg := agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "")
	step := agentStepWithAssert(&config.Assert{Stdout: &stdout})

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	content, err := AgentContentMap(cfg, step, ri)
	if err != nil {
		t.Fatalf("AgentContentMap: %v", err)
	}

	assertContentMap, ok := content["assert"].(map[string]any)
	if !ok {
		t.Fatalf("assert content = %#v, want a map", content["assert"])
	}

	if _, present := assertContentMap["tool_calls"]; present {
		t.Error("unset tool_calls must not appear in the hashed content")
	}
}

// TestAssertToolCallsHashBusts proves setting or changing tool_calls changes
// the step's hash — an assert defines success criteria, so it must bust the
// cache.
func TestAssertToolCallsHashBusts(t *testing.T) {
	t.Parallel()

	cfg := agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "")

	stdout := "posted"
	base := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{Stdout: &stdout}))

	withCalls := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{
		Stdout:    &stdout,
		ToolCalls: []config.ExpectedToolCall{{Name: "read_file"}},
	}))

	differentName := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{
		Stdout:    &stdout,
		ToolCalls: []config.ExpectedToolCall{{Name: "list_dir"}},
	}))

	withArgs := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{
		Stdout:    &stdout,
		ToolCalls: []config.ExpectedToolCall{{Name: "read_file", Args: map[string]string{"path": "a"}}},
	}))

	differentArgs := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{
		Stdout:    &stdout,
		ToolCalls: []config.ExpectedToolCall{{Name: "read_file", Args: map[string]string{"path": "b"}}},
	}))

	reordered := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{
		Stdout: &stdout,
		ToolCalls: []config.ExpectedToolCall{
			{Name: "list_dir"},
			{Name: "read_file"},
		},
	}))

	twoInOrder := mustAgentHash(t, cfg, agentStepWithAssert(&config.Assert{
		Stdout: &stdout,
		ToolCalls: []config.ExpectedToolCall{
			{Name: "read_file"},
			{Name: "list_dir"},
		},
	}))

	pairs := []struct {
		name string
		a, b string
	}{
		{"adding tool_calls", base, withCalls},
		{"changing a name", withCalls, differentName},
		{"adding args", withCalls, withArgs},
		{"changing an arg value", withArgs, differentArgs},
		{"reordering entries", twoInOrder, reordered},
	}

	for _, p := range pairs {
		if p.a == p.b {
			t.Errorf("%s should change the hash, but hashes matched", p.name)
		}
	}
}
