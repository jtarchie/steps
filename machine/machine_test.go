package machine

import (
	"strings"
	"testing"
)

func TestLoadSummarizeCritic(t *testing.T) {
	m, err := Load("../examples/summarize-critic/workflow.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Initial != "draft" {
		t.Errorf("initial = %q, want draft (first declared state)", m.Initial)
	}

	draft := m.State("draft")
	if draft == nil || draft.Agent == nil {
		t.Fatal("draft state missing or not an agent")
	}
	// Linear-flow default: draft has no transitions block -> next in doc order.
	if len(draft.Transitions) != 1 || draft.Transitions[0].To != "critique" {
		t.Errorf("draft transitions = %+v, want single fallback to critique", draft.Transitions)
	}
	if draft.Agent.Model != "ollama/qwen3:8b" {
		t.Errorf("draft model = %q (defaults cascade)", draft.Agent.Model)
	}
	if draft.Agent.MaxTurns != 2 {
		t.Errorf("draft max_turns = %d, want 2 from defaults.agent (machine-level cascade)", draft.Agent.MaxTurns)
	}
	if len(draft.Retry) != 2 {
		t.Errorf("draft retry = %+v, want engine default policies", draft.Retry)
	}
	if draft.Output.Compiled == nil {
		t.Error("draft output schema not compiled")
	}

	critique := m.State("critique")
	if critique == nil {
		t.Fatal("critique missing")
	}
	if len(critique.Transitions) != 3 {
		t.Fatalf("critique transitions = %d, want 3", len(critique.Transitions))
	}
	if critique.Transitions[0].Guard == nil || critique.Transitions[1].Guard == nil {
		t.Error("critique guards not compiled")
	}
	if !critique.Transitions[2].Fallback() {
		t.Error("critique last transition should be the fallback")
	}

	// Implicit terminals.
	for _, name := range []string{"done", "failed"} {
		s := m.State(name)
		if s == nil || !s.Terminal {
			t.Errorf("implicit terminal %q missing", name)
		}
	}
	if m.State("failed").Status != "failed" {
		t.Error("failed terminal should carry status failed")
	}

	// Publish (last declared) flows to done.
	pub := m.State("publish")
	if len(pub.Transitions) != 1 || pub.Transitions[0].To != "done" {
		t.Errorf("publish transitions = %+v, want fallback to done", pub.Transitions)
	}

	if m.Limits.MaxTransitions != 12 {
		t.Errorf("max_transitions = %d, want 12", m.Limits.MaxTransitions)
	}
	if m.Limits.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want engine default", m.Limits.Timeout)
	}
}

func TestLoadAdoptVariant(t *testing.T) {
	m, err := Load("../examples/summarize-critic-adopt/workflow.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	draft := m.State("draft")
	if draft.Agent.Adopt != "self" {
		t.Errorf("draft adopt = %q, want self", draft.Agent.Adopt)
	}
}

func TestTerseMachine(t *testing.T) {
	src := `
name: summarize
states:
  draft:
    agent: "Summarize in 3 bullets: {{ .ctx.article }}"
  publish:
    action: file.write
    input: {path: out/summary.md, content: "{{ .ctx.draft.text }}"}
`
	// No model anywhere in the YAML: the engine-level default (last rung of
	// the cascade) makes it valid.
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("terse machine without any model should fail without an engine default")
	}
	m, err := Parse([]byte(src), WithEngineDefaultModel("mock"))
	if err != nil {
		t.Fatalf("terse machine should be valid with engine default model: %v", err)
	}
	if m.Initial != "draft" {
		t.Errorf("initial = %q", m.Initial)
	}
	draft := m.State("draft")
	if draft.Agent.Prompt == "" {
		t.Error("scalar agent shorthand should become the prompt")
	}
	if !draft.Output.DefaultOutput() {
		t.Errorf("draft output = %+v, want default {text: string}", draft.Output.Schema)
	}
	if draft.Agent.Model != "mock" {
		t.Errorf("draft model = %q, want engine default", draft.Agent.Model)
	}
}

func TestAdoptMapForm(t *testing.T) {
	src := `
name: trim
defaults: {agent: {model: mock}}
states:
  work:
    agent:
      adopt: {from: self, last_turns: 6}
      prompt: "go"
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := m.State("work").Agent
	if a.Adopt != "self" || a.AdoptLastTurns != 6 {
		t.Errorf("adopt = %q last_turns = %d, want self/6", a.Adopt, a.AdoptLastTurns)
	}
}

func TestGenaiSchema(t *testing.T) {
	s := GenaiSchema(map[string]any{
		"score":  map[string]any{"type": "number"},
		"issues": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 3},
		"title":  "string",
	}, []string{"approve", "revise"})

	if s.Type != "OBJECT" {
		t.Errorf("root type = %q", s.Type)
	}
	if len(s.Required) != 4 { // score, issues, title + event
		t.Errorf("required = %v, want 4 entries", s.Required)
	}
	if s.Properties["score"].Type != "NUMBER" {
		t.Errorf("score type = %q", s.Properties["score"].Type)
	}
	if s.Properties["title"].Type != "STRING" {
		t.Errorf("scalar shorthand type = %q", s.Properties["title"].Type)
	}
	issues := s.Properties["issues"]
	if issues.Type != "ARRAY" || issues.Items == nil || issues.Items.Type != "STRING" {
		t.Errorf("issues schema = %+v", issues)
	}
	if issues.MaxItems == nil || *issues.MaxItems != 3 {
		t.Errorf("issues maxItems = %v, want 3", issues.MaxItems)
	}
	ev := s.Properties["event"]
	if ev == nil || len(ev.Enum) != 2 {
		t.Errorf("event enum = %+v", ev)
	}
}

func TestGuardCompileAndEval(t *testing.T) {
	p, err := CompileGuard(`output.score >= 8 && visits.draft < 3`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	env := GuardEnv()
	env["output"] = map[string]any{"score": 9.0}
	env["visits"] = map[string]int{"draft": 1}
	ok, err := EvalGuard(p, env)
	if err != nil || !ok {
		t.Errorf("eval = %v, %v; want true", ok, err)
	}

	if _, err := CompileGuard(`outputs.score > 1`); err == nil {
		t.Error("unknown identifier should fail at compile time")
	}
}

func TestValidationCatchesBadTransitions(t *testing.T) {
	src := `
name: bad
defaults: {agent: {model: mock}}
states:
  a:
    agent: "hi"
    transitions:
      - on: nope
        to: b
      - to: done
  b:
    agent: "bye"
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "not in output.events") {
		t.Errorf("want undeclared-event error, got %v", err)
	}
}

func TestValidationCatchesUnreachable(t *testing.T) {
	src := `
name: bad
defaults: {agent: {model: mock}}
states:
  a:
    agent: "hi"
    transitions: [{to: done}]
  orphan:
    agent: "never"
    transitions: [{to: done}]
`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("want unreachable error, got %v", err)
	}
}
