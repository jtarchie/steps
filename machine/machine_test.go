package machine

import (
	"strings"
	"testing"
)

func TestLoadSummarizeCritic(t *testing.T) {
	m, err := Load("../examples/summarize-critic/workflow.ts")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Initial != "draft" {
		t.Errorf("initial = %q, want draft (flow entry)", m.Initial)
	}

	draft := m.State("draft")
	if draft == nil || draft.Agent == nil {
		t.Fatal("draft state missing or not an agent")
	}
	// pipe adjacency: draft falls through to critique.
	if len(draft.Transitions) != 1 || draft.Transitions[0].To != "critique" {
		t.Errorf("draft transitions = %+v, want single fallback to critique", draft.Transitions)
	}
	if ref, _ := draft.Agent.Model.Static.(string); ref != "ollama/qwen3:8b" {
		t.Errorf("draft model = %v (top-level model sugar cascade)", draft.Agent.Model.Display())
	}
	if draft.Agent.MaxTurns != 2 {
		t.Errorf("draft maxTurns = %d, want 2 from flattened defaults", draft.Agent.MaxTurns)
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
		t.Fatalf("critique transitions = %d, want 3 (approve, revise, else)", len(critique.Transitions))
	}
	if !critique.Transitions[0].When.IsFn() || !critique.Transitions[1].When.IsFn() {
		t.Error("critique guards should be functions")
	}
	if !critique.Transitions[2].Fallback() {
		t.Error("critique last transition should be the else fallback")
	}

	escalate := m.State("escalate")
	if escalate.Human == nil || escalate.Human.OnTimeout != "failed" {
		t.Errorf("escalate timeout route = %+v, want failed via branch timeout key", escalate.Human)
	}

	publish := m.State("publish")
	if publish.Action == nil || publish.Action.Name != "file.write" {
		t.Errorf("publish should be write-sugar file.write, got %+v", publish.Action)
	}
	if len(publish.Transitions) != 1 || publish.Transitions[0].To != "done" {
		t.Errorf("publish transitions = %+v, want outgoing-edge default to done", publish.Transitions)
	}

	for _, name := range []string{"done", "failed"} {
		if s := m.State(name); s == nil || !s.Terminal {
			t.Errorf("implicit terminal %q missing", name)
		}
	}
	if m.Limits.MaxTransitions != 12 {
		t.Errorf("maxTransitions = %d, want 12", m.Limits.MaxTransitions)
	}
}

func TestLoadAdoptVariant(t *testing.T) {
	m, err := Load("../examples/summarize-critic-adopt/workflow.ts")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if a := m.State("draft").Agent.Adopt; a != "self" {
		t.Errorf("draft adopt = %q, want self", a)
	}
}

func TestTerseMachine(t *testing.T) {
	src := `
export default {
  name: "summarize",
  states: {
    draft: "Summarize the article in 3 bullets",
    publish: { write: "out/summary.md", content: ({ draft }) => draft.text },
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
	draft := m.State("draft")
	if draft.Agent == nil || draft.Agent.Prompt.IsZero() {
		t.Error("bare-string state should become an agent prompt")
	}
	if !draft.Output.DefaultOutput() {
		t.Errorf("draft output = %+v, want default {text: string}", draft.Output.Schema)
	}
	// No flow: linear declaration-order defaults apply.
	if to := draft.Transitions[0].To; to != "publish" {
		t.Errorf("draft flows to %q, want publish (linear default)", to)
	}
}

func TestStateOrderPreserved(t *testing.T) {
	src := `
export default {
  name: "ordered",
  model: "mock",
  states: {
    zebra: "one",
    alpha: "two",
    middle: "three",
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
export default {
  name: "trim",
  model: "mock",
  states: {
    work: { adopt: { from: "self", lastTurns: 6 }, prompt: "go" },
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
const triage = {
  prompt: "classify it",
  output: { severity: "enum(low, high)" },
  events: ["done_it"],
};
export default {
  name: "typo",
  model: "mock",
  states: { triage },
  flow: pipe(branch(triage, {
    done_it: when(({ output }) => output.sevrity === "high").to(done),
    else: done,
  })),
};`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("typo in guard should fail at load")
	}
	if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "severity") {
		t.Errorf("error should name the field and list available ones, got: %v", err)
	}
}

func TestContractCatchesUnknownDestructure(t *testing.T) {
	// Declaring input: buys strict checking of destructured parameters.
	src := `
const a = "one";
const b = { prompt: ({ aa }) => "prev said: " + aa.text };
export default {
  name: "typo2",
  model: "mock",
  input: { article: "string" },
  states: { a, b },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "{aa}") || !strings.Contains(err.Error(), "available") {
		t.Errorf("unknown destructured key should fail at load listing available keys, got: %v", err)
	}
}

func TestInfiniteGuardIsWarning(t *testing.T) {
	src := `
const a = { prompt: "one" };
export default {
  name: "loopy",
  model: "mock",
  states: { a },
  flow: pipe(branch(a, [
    when(() => { while (true) {} }).to(done),
    done,
  ])),
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

func TestGuardOnlyArrayBranch(t *testing.T) {
	src := `
const work = { prompt: "go", output: { n: "number" } };
export default {
  name: "arrayform",
  model: "mock",
  states: { work },
  flow: pipe(branch(work, [
    when(({ output }) => output.n > 1).to(done),
    fail,
  ])),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ts := m.State("work").Transitions
	if len(ts) != 2 || !ts[0].When.IsFn() || ts[1].To != "failed" || !ts[1].Fallback() {
		t.Errorf("array-form transitions = %+v", ts)
	}
}

func TestIncludePinsAssets(t *testing.T) {
	src := `
export default {
  name: "inc",
  model: "mock",
  states: {
    a: include("fixtures/article.txt"),
  },
};`
	m, err := parseSource([]byte(src), "../examples/summarize-critic")
	if err != nil {
		t.Fatalf("parse with include: %v", err)
	}
	if _, ok := m.Assets["fixtures/article.txt"]; !ok {
		t.Fatal("included file should be pinned as an asset")
	}
	m2, err := ParseWithAssets(m.Source, m.Assets)
	if err != nil {
		t.Fatalf("ParseWithAssets: %v", err)
	}
	if m2.Hash != m.Hash {
		t.Error("pinned rebuild should hash identically")
	}
}

func TestModelAliasErrors(t *testing.T) {
	src := `
export default {
  name: "aliased",
  models: { scout: "mock", senior: "mock" },
  states: {
    a: { model: "senoir", prompt: "hi" },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "scout, senior") {
		t.Errorf("unknown alias should list the valid aliases, got %v", err)
	}
}

func TestMovedToFlowKeysError(t *testing.T) {
	src := `
export default {
  name: "old",
  model: "mock",
  states: {
    a: { prompt: "hi", transitions: [{ to: "done" }] },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "moved to the flow") {
		t.Errorf("old transitions key should point at the flow, got %v", err)
	}
}

func TestUnknownStateKeyError(t *testing.T) {
	src := `
export default {
  name: "typo3",
  model: "mock",
  states: {
    a: { promt: "hi" },
  },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), `"promt"`) || !strings.Contains(err.Error(), "valid keys") {
		t.Errorf("unknown state key should list valid keys, got %v", err)
	}
}

func TestReservedNameCollision(t *testing.T) {
	src := `
export default {
  name: "shadow",
  model: "mock",
  input: { output: "string" },
  states: { a: "hi" },
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("input shadowing a reserved scope key should fail, got %v", err)
	}
}

func TestDoubleWiringError(t *testing.T) {
	src := `
const a = { prompt: "one" };
const b = { prompt: "two" };
export default {
  name: "dupe",
  model: "mock",
  states: { a, b },
  flow: pipe(a, b, a, done),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "wired more than once") {
		t.Errorf("double wiring should fail, got %v", err)
	}
}

func TestValidationCatchesBadEvent(t *testing.T) {
	src := `
const a = { prompt: "hi", output: { x: "string" } };
const b = { prompt: "bye" };
export default {
  name: "bad",
  model: "mock",
  states: { a, b },
  flow: pipe(branch(a, { nope: b, else: done })),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "not in output.events") {
		t.Errorf("want undeclared-event error, got %v", err)
	}
}

func TestValidationCatchesUnreachable(t *testing.T) {
	src := `
const a = { prompt: "hi" };
const orphan = { prompt: "never" };
export default {
  name: "bad",
  model: "mock",
  states: { a, orphan },
  flow: pipe(a),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("want unreachable error, got %v", err)
	}
}

func TestDedent(t *testing.T) {
	in := "\n    Line one\n      indented more\n    line three\n  "
	want := "Line one\n  indented more\nline three"
	if got := Dedent(in); got != want {
		t.Errorf("Dedent = %q, want %q", got, want)
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
	issues := s.Properties["issues"]
	if issues.Type != "ARRAY" || issues.MaxItems == nil || *issues.MaxItems != 3 {
		t.Errorf("issues schema = %+v", issues)
	}
	if ev := s.Properties["event"]; ev == nil || len(ev.Enum) != 2 {
		t.Errorf("event enum = %+v", s.Properties["event"])
	}
}
