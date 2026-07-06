package machine

import (
	"reflect"
	"strings"
	"testing"
)

// The builder is pure sugar: state(build) must lower to the SAME validated
// State an object literal produces. These tests parse a literal machine and its
// state(...) twin and assert the load-bearing fields match — proving the
// closure form buys dynamic construction without changing what the loader sees.

// literalSrc and builderSrc are the same three-state loop machine written two
// ways. Keeping the function bodies byte-identical is what makes Dyn.Src match.
const equivLiteralSrc = `
const draft = {
  prompt: ({ article }) => ` + "`Summarize ${article}`" + `,
  output: { summary: "string" },
  reasoning: "high",
};
const critique = {
  model: "reviewer",
  prompt: ({ draft }) => ` + "`Review ${draft.summary}`" + `,
  output: { score: "number" },
  events: ["approve", "revise"],
};
const decide = {
  human: ({ critique }) => ` + "`Score ${critique.score} — approve?`" + `,
  choices: { approved: "ship it", rejected: "no" },
  timeout: "1h",
};
const publish = { write: "out/x.md", content: ({ draft }) => draft.summary };
export default {
  name: "equiv",
  model: "mock",
  input: { article: { type: "string", required: true } },
  models: { reviewer: "mock" },
  states: { draft, critique, decide, publish },
  flow: pipe(
    loop(draft, { judge: critique, accept: ({ output }) => output.score >= 8, maxVisits: 3 }),
    branch(decide, { approved: publish, rejected: fail, timeout: fail }),
  ),
};`

const equivBuilderSrc = `
const draft = state("draft", s => {
  s.prompt(({ article }) => ` + "`Summarize ${article}`" + `);
  s.output({ summary: "string" });
  s.reasoning("high");
});
const critique = state(s => {
  s.model("reviewer");
  s.prompt(({ draft }) => ` + "`Review ${draft.summary}`" + `);
  s.output({ score: "number" });
  s.events("approve", "revise");
});
const decide = state(s => {
  s.human(({ critique }) => ` + "`Score ${critique.score} — approve?`" + `);
  s.choices({ approved: "ship it", rejected: "no" });
  s.timeout("1h");
});
const publish = state(s => s.write("out/x.md").content(({ draft }) => draft.summary));
export default {
  name: "equiv",
  model: "mock",
  input: { article: { type: "string", required: true } },
  models: { reviewer: "mock" },
  states: { draft, critique, decide, publish },
  flow: pipe(
    loop(draft, { judge: critique, accept: ({ output }) => output.score >= 8, maxVisits: 3 }),
    branch(decide, { approved: publish, rejected: fail, timeout: fail }),
  ),
};`

func TestBuilderEquivalence(t *testing.T) {
	lit, err := Parse([]byte(equivLiteralSrc))
	if err != nil {
		t.Fatalf("parse literal: %v", err)
	}
	built, err := Parse([]byte(equivBuilderSrc))
	if err != nil {
		t.Fatalf("parse builder: %v", err)
	}

	if lit.Initial != built.Initial {
		t.Errorf("initial: literal %q != builder %q", lit.Initial, built.Initial)
	}
	if len(lit.States) != len(built.States) {
		t.Fatalf("state count: literal %d != builder %d", len(lit.States), len(built.States))
	}
	for _, ls := range lit.States {
		bs := built.State(ls.Name)
		if bs == nil {
			t.Errorf("builder is missing state %q", ls.Name)
			continue
		}
		compareStates(t, ls, bs)
	}
}

// compareStates asserts two states agree on every load-bearing field — handler,
// contracts, and wiring. It compares Dyn by Src/Static (the goja fn pointers
// differ between the two parses, so reflect.DeepEqual on the whole state can't
// be used — the same reason flow_test.go asserts on When.Src).
func compareStates(t *testing.T, a, b *State) {
	t.Helper()
	if a.HandlerKind() != b.HandlerKind() {
		t.Errorf("%s: handler %q != %q", a.Name, a.HandlerKind(), b.HandlerKind())
	}
	if !reflect.DeepEqual(a.Output.Schema, b.Output.Schema) {
		t.Errorf("%s: output schema %v != %v", a.Name, a.Output.Schema, b.Output.Schema)
	}
	if !reflect.DeepEqual(a.Output.Events, b.Output.Events) {
		t.Errorf("%s: output events %v != %v", a.Name, a.Output.Events, b.Output.Events)
	}
	compareHandler(t, a, b)
	compareTransitions(t, a, b)
}

func compareHandler(t *testing.T, a, b *State) {
	t.Helper()
	switch {
	case a.Agent != nil && b.Agent != nil:
		dynEq(t, a.Name+".prompt", a.Agent.Prompt, b.Agent.Prompt)
		dynEq(t, a.Name+".model", a.Agent.Model, b.Agent.Model)
		if a.Agent.Reasoning != b.Agent.Reasoning {
			t.Errorf("%s: reasoning %q != %q", a.Name, a.Agent.Reasoning, b.Agent.Reasoning)
		}
	case a.Action != nil && b.Action != nil:
		if a.Action.Name != b.Action.Name {
			t.Errorf("%s: action %q != %q", a.Name, a.Action.Name, b.Action.Name)
		}
	case a.Human != nil && b.Human != nil:
		dynEq(t, a.Name+".human", a.Human.Prompt, b.Human.Prompt)
		if !reflect.DeepEqual(a.Human.Choices, b.Human.Choices) {
			t.Errorf("%s: choices %+v != %+v", a.Name, a.Human.Choices, b.Human.Choices)
		}
	}
}

func compareTransitions(t *testing.T, a, b *State) {
	t.Helper()
	if len(a.Transitions) != len(b.Transitions) {
		t.Fatalf("%s: %d transitions != %d", a.Name, len(a.Transitions), len(b.Transitions))
	}
	for i := range a.Transitions {
		at, bt := a.Transitions[i], b.Transitions[i]
		if at.On != bt.On || at.To != bt.To || at.When.Src != bt.When.Src {
			t.Errorf("%s: transition %d {on:%q when:%q to:%q} != {on:%q when:%q to:%q}",
				a.Name, i, at.On, at.When.Src, at.To, bt.On, bt.When.Src, bt.To)
		}
	}
}

func dynEq(t *testing.T, label string, a, b Dyn) {
	t.Helper()
	if a.Src != b.Src {
		t.Errorf("%s: Dyn.Src %q != %q", label, a.Src, b.Src)
	}
	if !reflect.DeepEqual(a.Static, b.Static) {
		t.Errorf("%s: Dyn.Static %v != %v", label, a.Static, b.Static)
	}
}

// TestBuilderDynamicConstruction is the motivating use case: a state assembled
// with a loop and a conditional — impossible in a single object literal.
func TestBuilderDynamicConstruction(t *testing.T) {
	src := `
const TOOLS = ["kb.search", "file.read"];
const STRICT = true;
const worker = state(s => {
  s.prompt("do the work");
  for (const t of TOOLS) s.tool(t);
  if (STRICT) s.reasoning("high");
});
export default { name: "dyn", model: "mock", states: { worker } };`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	w := m.State("worker")
	if w.Agent == nil {
		t.Fatalf("worker is not an agent state")
	}
	if len(w.Agent.Tools) != 2 || w.Agent.Tools[0].Name != "kb.search" || w.Agent.Tools[1].Name != "file.read" {
		t.Errorf("tools = %+v, want [kb.search file.read] appended by the loop", w.Agent.Tools)
	}
	if w.Agent.Reasoning != "high" {
		t.Errorf("reasoning = %q, want high (set by the conditional)", w.Agent.Reasoning)
	}
}

// TestBuilderToolsArrayForm: tools(arr) sets, tool(x) appends — both normalize.
func TestBuilderToolsArrayForm(t *testing.T) {
	src := `
const worker = state(s => s.prompt("x").tools(["a.one", "a.two"]).tool("a.three"));
export default { name: "arr", model: "mock", states: { worker } };`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tools := m.State("worker").Agent.Tools
	got := make([]string, 0, len(tools))
	for _, tr := range tools {
		got = append(got, tr.Name)
	}
	if !reflect.DeepEqual(got, []string{"a.one", "a.two", "a.three"}) {
		t.Errorf("tools = %v, want [a.one a.two a.three]", got)
	}
}

// TestBuilderUnknownMethod: a typo'd setter throws at load with a helpful
// message instead of a raw "undefined is not a function".
func TestBuilderUnknownMethod(t *testing.T) {
	src := `
const bad = state(s => s.promt("typo"));
export default { name: "bad", model: "mock", states: { bad } };`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected an error for the unknown builder method")
	}
	if !strings.Contains(err.Error(), "unknown state builder method 'promt'") {
		t.Errorf("error = %v, want it to name the unknown method", err)
	}
}

// TestBuilderNameMismatch: the optional name argument must match the states: key.
func TestBuilderNameMismatch(t *testing.T) {
	src := `
const draft = state("summary", s => s.prompt("x"));
export default { name: "mismatch", model: "mock", states: { draft } };`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected an error for the name/key mismatch")
	}
	if !strings.Contains(err.Error(), "must match the states: key") {
		t.Errorf("error = %v, want the name/key mismatch message", err)
	}
}

// TestBuilderNameMatch: a matching name argument loads cleanly.
func TestBuilderNameMatch(t *testing.T) {
	src := `
const draft = state("draft", s => s.prompt("x"));
export default { name: "match", model: "mock", states: { draft } };`
	_, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
}

// TestBuilderShadowing: a machine may still name a state const "state" — only
// state(...) CALLS reach the builder, so the plain object shadows harmlessly.
func TestBuilderShadowing(t *testing.T) {
	src := `
const state = { prompt: "I am a normal state named state" };
export default { name: "shadow", model: "mock", states: { state } };`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s := m.State("state"); s == nil || s.Agent == nil {
		t.Errorf("state named 'state' did not load as a normal agent state")
	}
}
