package machine

import (
	"strings"
	"testing"
)

// TestLoopUsesVerdict: a judge that declares verdict: needs no accept: on the
// loop — the verdict becomes the accept edge, lowered like a hand-written one.
func TestLoopUsesVerdict(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const judge = { prompt: "score it", output: { score: "number" }, verdict: ({ output }) => output.score >= 8 };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "verdict-loop",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(
    loop(work, { judge: judge, maxVisits: 3 }),
    ship,
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := m.State("judge")
	if len(j.Transitions) != 3 {
		t.Fatalf("judge transitions = %d, want 3", len(j.Transitions))
	}
	accept := j.Transitions[0]
	if !accept.When.IsFn() || accept.To != "ship" {
		t.Errorf("accept edge = %+v, want the verdict guard -> ship", accept)
	}
	if !strings.Contains(accept.When.Src, "output.score") {
		t.Errorf("accept guard src = %q, want the judge's verdict", accept.When.Src)
	}
}

func TestLoopAcceptVerdictConflict(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const judge = { prompt: "score", output: { score: "number" }, verdict: ({ output }) => output.score >= 8 };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "conflict",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(loop(work, { judge: judge, accept: ({ output }) => output.score >= 9, maxVisits: 3 }), ship),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "declare the acceptance test once") {
		t.Fatalf("err = %v, want a declare-once conflict error", err)
	}
}

func TestLoopNoAcceptNoVerdict(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const judge = { prompt: "score", output: { score: "number" } };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "none",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(loop(work, { judge: judge, maxVisits: 3 }), ship),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "no acceptance test") {
		t.Fatalf("err = %v, want a no-acceptance-test error", err)
	}
}

func TestVerdictMustBeFunction(t *testing.T) {
	src := `
const judge = { prompt: "score", verdict: true };
export default { name: "bad", model: "mock", states: { judge }, flow: pipe(judge, done) };`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "verdict must be a function") {
		t.Fatalf("err = %v, want verdict-must-be-a-function", err)
	}
}

func TestVerdictTypoCaughtByDryRun(t *testing.T) {
	// A verdict referencing a field the schema does not have fails the load.
	src := `
const work = { prompt: "produce" };
const judge = { prompt: "score", output: { score: "number" }, verdict: ({ output }) => output.scoer >= 8 };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "typo",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(loop(work, { judge: judge, maxVisits: 3 }), ship),
};`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected the verdict typo to fail the load")
	}
}

func TestVerdictRejectedOnHumanState(t *testing.T) {
	src := `
const gate = { human: "ok?", choices: { approved: "yes", rejected: "no" }, verdict: ({ output }) => true };
export default {
  name: "bad-verdict",
  model: "mock",
  states: { gate },
  flow: pipe(branch(gate, { approved: done, rejected: fail })),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "verdict") {
		t.Fatalf("err = %v, want verdict-not-on-human error", err)
	}
}
