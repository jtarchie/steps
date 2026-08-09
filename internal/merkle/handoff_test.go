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
	sending := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", Handoff: &config.HandoffSpec{Note: true}})
	receiving := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", HandoffNoteFrom: []string{"planner"}})
	fromOther := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "do it", HandoffNoteFrom: []string{"coder"}})

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

// TestContextQualifyIsIdentity proves qualify: changes a step's hash.
//
// It decides WHERE the step's writes land — its own per-cell scope, merged
// under a key naming the cell, rather than the run scope under the plain key —
// so two steps recording under different key names are not the same step.
//
// Left out, adding qualify: to a matrix of task cells was a CACHE HIT: the
// cells skipped, no per-cell scope was ever written, the join merged nothing,
// and the author read back the old unqualified key with no error at all. That
// is the silent key-shape change qualify: exists to eliminate, reintroduced by
// the hash.
func TestContextQualifyIsIdentity(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")
	step := func(spec *config.ContextSpec) config.Step {
		return config.Step{Agent: "reviewer", Prompt: "do it", Context: spec}
	}

	write := mustAgentHash(t, cfg, step(&config.ContextSpec{Write: true}))
	qualified := mustAgentHash(t, cfg, step(&config.ContextSpec{Write: true, Qualify: true}))

	if write == qualified {
		t.Error("adding context qualify: must change an agent step's hash")
	}
}

// TestContextQualifyIsIdentityForATask is the half that actually bites.
//
// An agent step is never cacheable, so hashing its declaration alone would be
// a no-op — while a TASK cell of a matrix is the one thing a rerun can skip
// (see CellHash). TaskNodeContent did not fold context: at all, so the agent
// fix above changed nothing about the observed bug: the cells still skipped
// and the join still merged empty scopes.
func TestContextQualifyIsIdentityForATask(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")
	hash := func(spec *config.ContextSpec) string {
		t.Helper()

		step := config.Step{Task: "record", Run: "true", Context: spec}

		rt, err := cfg.ResolveTask(step)
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}

		content, err := TaskNodeContent(cfg, step, rt)
		if err != nil {
			t.Fatalf("TaskNodeContent: %v", err)
		}

		out, err := HashNode(NodeKindTask, content, "parent")
		if err != nil {
			t.Fatalf("HashNode: %v", err)
		}

		return out
	}

	if hash(&config.ContextSpec{Write: true}) == hash(&config.ContextSpec{Write: true, Qualify: true}) {
		t.Error("adding context qualify: must change a task cell's hash; without it the cells skip and record nothing")
	}

	if hash(nil) == hash(&config.ContextSpec{Write: true}) {
		t.Error("context: write changes whether the runner collects the context/ dir, so it must change a task's hash")
	}
}

// TestContextQualifyHashStabilityWhenUnset is the other half: value-gating, so
// every pipeline that does not set qualify: hashes byte-identically to before
// the field existed and nothing already cached re-runs.
func TestContextQualifyHashStabilityWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := agentCfg(nil, "")
	step := config.Step{Agent: "reviewer", Prompt: "do it", Context: &config.ContextSpec{Write: true}}

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	content, err := AgentContentMap(cfg, step, ri)
	if err != nil {
		t.Fatalf("AgentContentMap: %v", err)
	}

	entry, ok := content["context"].(map[string]any)
	if !ok {
		t.Fatalf("content[context] = %#v, want a map", content["context"])
	}

	if _, present := entry["qualify"]; present {
		t.Error("unset qualify must not appear in the hashed content")
	}
}
