package machine

import (
	"reflect"
	"strings"
	"testing"
)

// The machine(name, build) block is aasm-style sugar: it lowers to the same
// validated Machine an object export would. These tests assert the event-set
// lowering (transitions, fan-in, auto-declared events) and that it defers to the
// existing validation rather than reimplementing them.

// TestMachineLowering: events + guards + a trailing fallback lower to the exact
// per-state Transition list, and using an event auto-declares it on the state.
func TestMachineLowering(t *testing.T) {
	src := `
export default machine("m", (m) => {
  m.model("mock");
  const a = m.state("a", s => s.prompt("do a").output({ score: "number" }));
  const b = m.state("b", s => s.prompt("do b").output({ score: "number" }));
  const c = m.state("c", s => s.write("out/x").content("done"));
  m.start(a);
  m.event("approve", { from: a, to: c, when: ({ output }) => output.score >= 8 });
  m.event("revise",  { from: a, to: b });
  m.always(a, b);
  m.step(b, c);
});`
	mm, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if mm.Initial != "a" {
		t.Errorf("initial = %q, want a (m.start)", mm.Initial)
	}

	a := mm.State("a")
	if len(a.Transitions) != 3 {
		t.Fatalf("a transitions = %d, want 3 (approve, revise, fallback)", len(a.Transitions))
	}
	if a.Transitions[0].On != "approve" || !a.Transitions[0].When.IsFn() || a.Transitions[0].To != "c" {
		t.Errorf("edge 0 = %+v, want guarded approve -> c", a.Transitions[0])
	}
	if a.Transitions[1].On != "revise" || !a.Transitions[1].When.IsZero() || a.Transitions[1].To != "b" {
		t.Errorf("edge 1 = %+v, want bare revise -> b", a.Transitions[1])
	}
	if !a.Transitions[2].Fallback() || a.Transitions[2].To != "b" {
		t.Errorf("edge 2 = %+v, want unconditional fallback -> b", a.Transitions[2])
	}

	// Using approve/revise auto-declared them on a — no events: line needed.
	if !reflect.DeepEqual(a.Output.Events, []string{"approve", "revise"}) {
		t.Errorf("a.output.events = %v, want [approve revise] (auto-injected)", a.Output.Events)
	}

	b := mm.State("b")
	if len(b.Transitions) != 1 || b.Transitions[0].To != "c" {
		t.Errorf("b transitions = %+v, want single fallback -> c (m.step)", b.Transitions)
	}
}

// TestMachineFanIn: one event edge with from: [a, b] wires a transition on BOTH
// states — aasm's `transitions from: [...]` in a single line.
func TestMachineFanIn(t *testing.T) {
	src := `
export default machine("m", (m) => {
  m.model("mock");
  const a = m.state("a", s => s.prompt("a").output({ score: "number" }));
  const b = m.state("b", s => s.prompt("b").output({ score: "number" }));
  const c = m.state("c", s => s.write("o").content("x"));
  m.start(a);
  m.event("go", { from: [a, b], to: c });
  m.always(a, b);
  m.always(b, c);
});`
	mm, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, name := range []string{"a", "b"} {
		st := mm.State(name)
		if st.Transitions[0].On != "go" || st.Transitions[0].To != "c" {
			t.Errorf("%s edge 0 = %+v, want go -> c (fan-in)", name, st.Transitions[0])
		}
		if !contains(st.Output.Events, "go") {
			t.Errorf("%s.output.events = %v, want it to include go", name, st.Output.Events)
		}
	}
}

// TestMachineUnknownVerb: a typo'd builder verb throws a helpful error at load.
func TestMachineUnknownVerb(t *testing.T) {
	src := `export default machine("m", (m) => { m.nope(); });`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected an error for the unknown builder verb")
	}
	if !strings.Contains(err.Error(), "unknown machine builder method 'nope'") {
		t.Errorf("error = %v, want it to name the unknown verb", err)
	}
}

// TestMachineMissingFallback: an agent state with only event edges and no
// m.always surfaces the EXISTING validate.go fallback error — the lowering
// defers to validation, it does not reimplement it.
func TestMachineMissingFallback(t *testing.T) {
	src := `
export default machine("m", (m) => {
  m.model("mock");
  const a = m.state("a", s => s.prompt("a").output({ score: "number" }));
  const c = m.state("c", s => s.write("o").content("x"));
  m.start(a);
  m.event("go", { from: a, to: c });
});`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected a validation error for the missing fallback")
	}
	if !strings.Contains(err.Error(), "unconditional fallback") {
		t.Errorf("error = %v, want the missing-fallback validation error", err)
	}
}

// TestMachineHumanGateNoFallback: a human gate needs no fallback (it routes on
// resume events), timeout lowers to Human.OnTimeout, and auto-inject is skipped
// for gates (they have no output.events).
func TestMachineHumanGateNoFallback(t *testing.T) {
	src := `
export default machine("m", (m) => {
  m.model("mock");
  const a = m.state("a", s => s.prompt("a").output({ score: "number" }));
  const g = m.state("g", s => s.human("approve?").choices({ approved: "yes", rejected: "no" }).timeout("1h"));
  const c = m.state("c", s => s.write("o").content("x"));
  m.start(a);
  m.always(a, g);
  m.event("approved", { from: g, to: c });
  m.event("rejected", { from: g, to: fail });
  m.timeout(g, fail);
});`
	mm, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := mm.State("g")
	if g.Human == nil || g.Human.OnTimeout != "failed" {
		t.Errorf("g.Human.OnTimeout = %v, want failed (m.timeout)", g.Human)
	}
	if len(g.Transitions) != 2 || g.Transitions[0].On != "approved" || g.Transitions[0].To != "c" {
		t.Errorf("g transitions = %+v, want approved -> c, rejected -> failed", g.Transitions)
	}
	if len(g.Output.Events) != 0 {
		t.Errorf("g.output.events = %v, want empty (auto-inject skips gates)", g.Output.Events)
	}
}
