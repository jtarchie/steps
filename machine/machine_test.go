package machine

import (
	"strings"
	"testing"
)

func TestLoadSummarizeCritic(t *testing.T) {
	m, err := Load("../examples/summarize-critic/workflow.js")
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
	// Linear-flow default: draft has no transitions -> next in declaration order.
	if len(draft.Transitions) != 1 || draft.Transitions[0].To != "critique" {
		t.Errorf("draft transitions = %+v, want single fallback to critique", draft.Transitions)
	}
	if ref, _ := draft.Agent.Model.Static.(string); ref != "ollama/qwen3:8b" {
		t.Errorf("draft model = %v (defaults cascade)", draft.Agent.Model.Display())
	}
	if draft.Agent.MaxTurns != 2 {
		t.Errorf("draft maxTurns = %d, want 2 from defaults.agent", draft.Agent.MaxTurns)
	}
	if !draft.Agent.Prompt.IsFn() {
		t.Error("draft prompt should be a function")
	}
	if len(draft.Retry) != 2 {
		t.Errorf("draft retry = %+v, want engine default policies", draft.Retry)
	}
	if draft.Output.Compiled == nil {
		t.Error("draft output schema not compiled")
	}

	critique := m.State("critique")
	if len(critique.Transitions) != 3 {
		t.Fatalf("critique transitions = %d, want 3", len(critique.Transitions))
	}
	if !critique.Transitions[0].When.IsFn() || !critique.Transitions[1].When.IsFn() {
		t.Error("critique guards should be functions")
	}
	if !critique.Transitions[2].Fallback() {
		t.Error("critique last transition should be the fallback")
	}

	for _, name := range []string{"done", "failed"} {
		s := m.State(name)
		if s == nil || !s.Terminal {
			t.Errorf("implicit terminal %q missing", name)
		}
	}
	if m.Limits.MaxTransitions != 12 {
		t.Errorf("maxTransitions = %d, want 12", m.Limits.MaxTransitions)
	}
	if m.Limits.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want engine default", m.Limits.Timeout)
	}
}

func TestLoadAdoptVariant(t *testing.T) {
	m, err := Load("../examples/summarize-critic-adopt/workflow.js")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if a := m.State("draft").Agent.Adopt; a != "self" {
		t.Errorf("draft adopt = %q, want self", a)
	}
}

func TestTerseMachine(t *testing.T) {
	src := `
module.exports = {
  name: "summarize",
  states: {
    draft: { agent: ({ctx}) => "Summarize in 3 bullets: " + ctx.article },
    publish: {
      action: "file.write",
      input: { path: "out/summary.md", content: ({ctx}) => ctx.draft.text },
    },
  },
};`
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
	if !m.State("draft").Output.DefaultOutput() {
		t.Errorf("draft output = %+v, want default {text: string}", m.State("draft").Output.Schema)
	}
}

func TestStateOrderPreserved(t *testing.T) {
	src := `
module.exports = {
  name: "ordered",
  defaults: { agent: { model: "mock" } },
  states: {
    zebra: { agent: "one" },
    alpha: { agent: "two" },
    middle: { agent: "three" },
  },
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Initial != "zebra" {
		t.Errorf("initial = %q, want zebra (declaration order)", m.Initial)
	}
	if to := m.State("zebra").Transitions[0].To; to != "alpha" {
		t.Errorf("zebra flows to %q, want alpha (declaration order, not lexical)", to)
	}
}

func TestAdoptObjectForm(t *testing.T) {
	src := `
module.exports = {
  name: "trim",
  defaults: { agent: { model: "mock" } },
  states: {
    work: { agent: { adopt: { from: "self", lastTurns: 6 }, prompt: "go" } },
  },
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := m.State("work").Agent
	if a.Adopt != "self" || a.AdoptLastTurns != 6 {
		t.Errorf("adopt = %q lastTurns = %d, want self/6", a.Adopt, a.AdoptLastTurns)
	}
}

func TestDryRunCatchesUnknownFields(t *testing.T) {
	src := `
module.exports = {
  name: "typo",
  defaults: { agent: { model: "mock" } },
  states: {
    triage: {
      agent: "classify",
      output: { schema: { severity: "enum(low, high)" }, events: ["done_it"] },
      transitions: [
        { on: "done_it", when: ({output}) => output.sevrity === "high", to: "done" },
        { to: "done" },
      ],
    },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("typo in guard should fail at load")
	}
	if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "severity") {
		t.Errorf("error should name the field and list available ones, got: %v", err)
	}
}

func TestDryRunUnknownCtxState(t *testing.T) {
	// Declaring input: buys strict ctx checking; without it, run inputs are
	// unknowable and ctx tolerates unknown keys.
	src := `
module.exports = {
  name: "typo2",
  input: { article: { type: "string" } },
  defaults: { agent: { model: "mock" } },
  states: {
    a: { agent: "one" },
    b: { agent: ({ctx}) => "prev said: " + ctx.aa.text },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "unknown field ctx.aa") {
		t.Errorf("unknown ctx state should fail at load listing states, got: %v", err)
	}
}

func TestInfiniteGuardIsWarning(t *testing.T) {
	src := `
module.exports = {
  name: "loopy",
  defaults: { agent: { model: "mock" } },
  states: {
    a: {
      agent: "one",
      transitions: [
        { when: () => { while (true) {} }, to: "done" },
        { to: "done" },
      ],
    },
  },
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("infinite guard should load (warning, not fatal): %v", err)
	}
	_, warnings := DryRun(m)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "exceeded") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an interrupt warning, got %v", warnings)
	}
}

func TestIncludePinsAssets(t *testing.T) {
	src := `
module.exports = {
  name: "inc",
  defaults: { agent: { model: "mock" } },
  states: {
    a: { agent: include("fixtures/article.txt") },
  },
};`
	m, err := parseSource([]byte(src), "../examples/summarize-critic")
	if err != nil {
		t.Fatalf("parse with include: %v", err)
	}
	if _, ok := m.Assets["fixtures/article.txt"]; !ok {
		t.Fatalf("included file should be pinned as an asset, got %v", mapKeys(m.Assets))
	}
	// Resume path: rebuild from pinned source + assets with no filesystem.
	m2, err := ParseWithAssets(m.Source, m.Assets)
	if err != nil {
		t.Fatalf("ParseWithAssets: %v", err)
	}
	if m2.Hash != m.Hash {
		t.Error("pinned rebuild should hash identically")
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestModelAliasErrors(t *testing.T) {
	src := `
module.exports = {
  name: "aliased",
  models: { scout: "mock", senior: "mock" },
  states: {
    a: { agent: { model: "senoir", prompt: "hi" } },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "scout, senior") {
		t.Errorf("unknown alias should list the valid aliases, got %v", err)
	}
}

func TestValidationCatchesBadTransitions(t *testing.T) {
	src := `
module.exports = {
  name: "bad",
  defaults: { agent: { model: "mock" } },
  states: {
    a: {
      agent: "hi",
      transitions: [{ on: "nope", to: "b" }, { to: "done" }],
    },
    b: { agent: "bye" },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "not in output.events") {
		t.Errorf("want undeclared-event error, got %v", err)
	}
}

func TestValidationCatchesUnreachable(t *testing.T) {
	src := `
module.exports = {
  name: "bad",
  defaults: { agent: { model: "mock" } },
  states: {
    a: { agent: "hi", transitions: "done" },
    orphan: { agent: "never", transitions: "done" },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("want unreachable error, got %v", err)
	}
}

func TestSchemaShorthand(t *testing.T) {
	frag, err := NormalizeSchemaFragment(map[string]any{
		"risk":  "enum(low, medium, high)",
		"tags":  "string[]",
		"leads": []any{map[string]any{"where": "string", "concern": "string"}},
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	props := frag["properties"].(map[string]any)
	risk := props["risk"].(map[string]any)
	if enum, _ := risk["enum"].([]any); len(enum) != 3 {
		t.Errorf("risk enum = %v", risk)
	}
	pipe, err := NormalizeSchemaFragment("enum(a|b|c)")
	if err != nil || len(pipe["enum"].([]any)) != 3 {
		t.Errorf("pipe enum = %v, %v", pipe, err)
	}
	tags := props["tags"].(map[string]any)
	if tags["type"] != "array" || tags["items"].(map[string]any)["type"] != "string" {
		t.Errorf("tags = %v", tags)
	}
	leads := props["leads"].(map[string]any)
	if leads["type"] != "array" || leads["items"].(map[string]any)["type"] != "object" {
		t.Errorf("leads = %v", leads)
	}
	if _, err := NormalizeSchemaFragment("strng"); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Errorf("typo should produce a friendly error, got %v", err)
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
	if len(s.Required) != 4 {
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
