package machine

import (
	"strings"
	"testing"
)

// TestLoopLowering: loop() is pure sugar — the judge owns exactly
// [accept -> then, visits budget -> revise, fallback -> exhausted], with
// then defaulting to the pipe successor, revise to the body's entry, and
// exhausted to failed.
func TestLoopLowering(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const judge = { prompt: "score it", output: { score: "number" } };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "loop-lowering",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(
    loop(work, {
      judge: judge,
      accept: ({ output }) => output.score >= 8,
      maxVisits: 3,
    }),
    ship,
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Initial != "work" {
		t.Errorf("initial = %q, want work (the loop body's entry)", m.Initial)
	}

	work := m.State("work")
	if len(work.Transitions) != 1 || work.Transitions[0].To != "judge" {
		t.Errorf("work transitions = %+v, want single fallback to judge", work.Transitions)
	}

	j := m.State("judge")
	if len(j.Transitions) != 3 {
		t.Fatalf("judge transitions = %d, want 3", len(j.Transitions))
	}
	if !j.Transitions[0].When.IsFn() || j.Transitions[0].To != "ship" {
		t.Errorf("accept edge = %+v, want guarded -> ship (the pipe successor)", j.Transitions[0])
	}
	if src := j.Transitions[1].When.Src; src != "({ visits }) => visits.judge < 3" {
		t.Errorf("budget guard src = %q, want the synthesized visits guard", src)
	}
	if j.Transitions[1].To != "work" {
		t.Errorf("revise edge -> %q, want work (the body's entry)", j.Transitions[1].To)
	}
	if !j.Transitions[2].Fallback() || j.Transitions[2].To != "failed" {
		t.Errorf("exhausted edge = %+v, want unconditional fallback -> failed", j.Transitions[2])
	}
}

// TestLoopExplicitRoutes: nested loops as then targets, an ACTION state as
// the judge, an explicit revise that re-enters upstream of the body, and
// catch edges wired like branch's.
func TestLoopExplicitRoutes(t *testing.T) {
	src := `
const gen = { prompt: "write code" };
const review = { prompt: "review it", output: { score: "number" } };
const write = { write: "out/f.txt", content: ({ gen }) => gen.text };
const build = {
  action: "exec.run",
  input: { cmd: "true" },
  output: { ok: "boolean", exit_code: "number", stdout: "string", stderr: "string", cmd: "string" },
};
const escalate = { human: "accept anyway?", timeout: "1h" };
export default {
  name: "nested-loops",
  model: "mock",
  states: { gen, review, write, build, escalate },
  flow: pipe(
    loop(gen, {
      judge: review,
      accept: ({ output }) => output.score >= 8,
      maxVisits: 5,
      catch: { budget_exceeded: escalate },
      exhausted: branch(escalate, { approved: write, rejected: fail, timeout: fail }),
      then: loop(write, {
        judge: build,
        accept: ({ output }) => output.ok,
        revise: gen,
        maxVisits: 4,
        then: done,
        exhausted: fail,
      }),
    }),
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	t.Run("review routes and catch", func(t *testing.T) { checkReviewRoutesAndCatch(t, m) })
	t.Run("build routes with explicit revise upstream", func(t *testing.T) { checkBuildExplicitRevise(t, m) })
	t.Run("write falls through to build", func(t *testing.T) { checkWriteFallsThroughToBuild(t, m) })
	t.Run("escalate timeout falls through to failed", func(t *testing.T) { checkEscalateTimeout(t, m) })
}

func checkReviewRoutesAndCatch(t *testing.T, m *Machine) {
	t.Helper()
	review := m.State("review")
	if got := []string{review.Transitions[0].To, review.Transitions[1].To, review.Transitions[2].To}; got[0] != "write" || got[1] != "gen" || got[2] != "escalate" {
		t.Errorf("review routes = %v, want [write gen escalate]", got)
	}
	if len(review.Catch) != 1 || review.Catch[0].To != "escalate" || review.Catch[0].Match[0] != "budget_exceeded" {
		t.Errorf("review catch = %+v, want budget_exceeded -> escalate", review.Catch)
	}
}

func checkBuildExplicitRevise(t *testing.T, m *Machine) {
	t.Helper()
	build := m.State("build")
	if src := build.Transitions[1].When.Src; src != "({ visits }) => visits.build < 4" {
		t.Errorf("inner budget guard src = %q", src)
	}
	if got := []string{build.Transitions[0].To, build.Transitions[1].To, build.Transitions[2].To}; got[0] != "done" || got[1] != "gen" || got[2] != "failed" {
		t.Errorf("build routes = %v, want [done gen failed] (explicit revise re-enters upstream)", got)
	}
}

func checkWriteFallsThroughToBuild(t *testing.T, m *Machine) {
	t.Helper()
	if w := m.State("write"); len(w.Transitions) != 1 || w.Transitions[0].To != "build" {
		t.Errorf("write transitions = %+v, want fallback to build (inner body)", w.Transitions)
	}
}

func checkEscalateTimeout(t *testing.T, m *Machine) {
	t.Helper()
	if esc := m.State("escalate"); esc.Human == nil || esc.Human.OnTimeout != "failed" {
		t.Errorf("escalate timeout route = %+v, want failed", esc.Human)
	}
}

// TestLoopErrors: every misuse fails at load, before any token is spent.
func TestLoopErrors(t *testing.T) {
	// wrap builds a machine whose flow is the given loop expression over the
	// standard states work/judge/ship/gate.
	wrap := func(flow string) string {
		return `
const work = { prompt: "produce" };
const judge = { prompt: "score", output: { score: "number" } };
const ship = { write: "out/x.txt", content: "done" };
const gate = { human: "ok?" };
export default {
  name: "loop-errors",
  model: "mock",
  states: { work, judge, ship, gate },
  flow: ` + flow + `,
};`
	}
	valid := `judge: judge, accept: ({ output }) => output.score >= 8, maxVisits: 3`

	cases := []struct {
		name, flow, want string
	}{
		{"unknown option", `pipe(loop(work, { ` + valid + `, bogus: 1 }), ship)`, `unknown option "bogus"`},
		{"judge missing", `pipe(loop(work, { accept: () => true, maxVisits: 3 }), ship)`, "judge must be a registered state"},
		{"judge is a human gate", `pipe(loop(work, { judge: gate, accept: () => true, maxVisits: 3 }), ship)`, "is a human gate"},
		{"judge wired twice", `pipe(branch(judge, { else: ship }), loop(work, { ` + valid + ` }), ship)`, "wired more than once"},
		{"body terminal", `pipe(loop(done, { ` + valid + ` }), ship)`, "body cannot be a terminal state"},
		{"self-judging", `pipe(loop(judge, { ` + valid + ` }), ship)`, "cannot also be the body"},
		{"accept missing", `pipe(loop(work, { judge: judge, maxVisits: 3 }), ship)`, "accept must be a function"},
		{"accept not a function", `pipe(loop(work, { judge: judge, accept: true, maxVisits: 3 }), ship)`, "accept must be a function"},
		{"maxVisits missing", `pipe(loop(work, { judge: judge, accept: () => true }), ship)`, "maxVisits is required"},
		{"maxVisits zero", `pipe(loop(work, { judge: judge, accept: () => true, maxVisits: 0 }), ship)`, "maxVisits must be >= 1"},
		{"maxVisits not a number", `pipe(loop(work, { judge: judge, accept: () => true, maxVisits: "3" }), ship)`, "maxVisits must be a number"},
		{"then and pipe successor", `pipe(loop(work, { ` + valid + `, then: ship }), ship)`, "AND the pipe continues"},
		{"then missing at pipe end", `loop(work, { ` + valid + ` })`, "needs a then"},
		{"catch not an object", `pipe(loop(work, { ` + valid + `, catch: 5 }), ship)`, "catch must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(wrap(tc.flow)))
			if err == nil {
				t.Fatalf("expected a load error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestMaxTurnsDefaultByTools: the engine default is conditional — a tool-use
// loop needs room to iterate, a tool-less state makes one call per turn.
func TestMaxTurnsDefaultByTools(t *testing.T) {
	src := `
const chat = { prompt: "hi" };
const worker = { prompt: "read then answer", tools: ["file.read"] };
export default { name: "turns", model: "mock", states: { chat, worker } };`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m.State("chat").Agent.MaxTurns; got != DefaultMaxTurnsToolless {
		t.Errorf("tool-less maxTurns = %d, want %d", got, DefaultMaxTurnsToolless)
	}
	if got := m.State("worker").Agent.MaxTurns; got != DefaultMaxTurns {
		t.Errorf("tool-ful maxTurns = %d, want %d", got, DefaultMaxTurns)
	}

	// defaults.maxTurns still wins over both engine rungs.
	src = `
const chat = { prompt: "hi" };
const worker = { prompt: "read then answer", tools: ["file.read"] };
export default { name: "turns", model: "mock", defaults: { maxTurns: 4 }, states: { chat, worker } };`
	m, err = Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, name := range []string{"chat", "worker"} {
		if got := m.State(name).Agent.MaxTurns; got != 4 {
			t.Errorf("%s maxTurns = %d, want 4 from defaults", name, got)
		}
	}
}

// TestMaxInputTokensDefault: the input cap is default-on (the engine rung),
// an author's 0 disables it at either rung, and implicit distill states are
// exempt — the distiller is where the big payload is supposed to appear.
func TestMaxInputTokensDefault(t *testing.T) {
	src := `
const a = { prompt: "hi" };
const b = { prompt: "ho", maxInputTokens: 0 };
const c = { prompt: "he", maxInputTokens: 123 };
export default { name: "caps", model: "mock", states: { a, b, c } };`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for name, want := range map[string]int{"a": DefaultMaxInputTokens, "b": 0, "c": 123} {
		got := m.State(name).Agent.MaxInputTokens
		if got == nil || *got != want {
			t.Errorf("%s maxInputTokens = %v, want %d", name, got, want)
		}
	}

	// Machine-wide off.
	src = `
const a = { prompt: "hi" };
export default { name: "caps-off", model: "mock", defaults: { maxInputTokens: 0 }, states: { a } };`
	m, err = Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m.State("a").Agent.MaxInputTokens; got == nil || *got != 0 {
		t.Errorf("machine-wide off: maxInputTokens = %v, want 0", got)
	}

	// Distill states stay uncapped even under the engine default.
	src = `
const use = {
  distill: { article: { for: "the key claims only", maxTokens: 80 } },
  prompt: ({ article }) => "Summarize:\n" + article,
};
export default { name: "caps-distill", input: { article: "string" }, model: "mock", states: { use } };`
	m, err = Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m.State("use#article").Agent.MaxInputTokens; got != nil {
		t.Errorf("distill state maxInputTokens = %d, want nil (exempt)", *got)
	}
	if got := m.State("use").Agent.MaxInputTokens; got == nil || *got != DefaultMaxInputTokens {
		t.Errorf("consumer maxInputTokens = %v, want the engine default", got)
	}

	// A slice that cannot fit under its consumer's cap is a load error.
	src = `
const use = {
  maxInputTokens: 100,
  distill: { article: { for: "the key claims only", maxTokens: 200 } },
  prompt: ({ article }) => "Summarize:\n" + article,
};
export default { name: "caps-conflict", input: { article: "string" }, model: "mock", states: { use } };`
	if _, err = Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "does not fit under the consumer's maxInputTokens") {
		t.Errorf("slice-over-cap error = %v, want the fit check to fire", err)
	}
}
