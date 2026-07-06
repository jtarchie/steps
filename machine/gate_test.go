package machine

import (
	"strings"
	"testing"
)

// TestGateApproveShorthand: gate("x", { approve: T }) synthesizes a human
// state gate#x with approved -> T, rejected -> failed, and a single-choice
// alphabet covering both, plus timeout -> failed when a duration is set.
func TestGateApproveShorthand(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "gate-approve",
  model: "mock",
  states: { work, ship },
  flow: pipe(
    branch(work, {
      else: gate("escalate", {
        prompt: ({ work }) => "ship " + work.text + "?",
        approve: ship,
        timeout: "1h",
      }),
    }),
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := m.State("gate#escalate")
	if g == nil {
		t.Fatal("synthesized state gate#escalate not found")
	}
	if !g.Gate || g.Human == nil {
		t.Fatalf("gate#escalate = %+v, want a Gate human state", g)
	}
	if len(g.Transitions) != 2 ||
		g.Transitions[0].On != "approved" || g.Transitions[0].To != "ship" ||
		g.Transitions[1].On != "rejected" || g.Transitions[1].To != "failed" {
		t.Errorf("transitions = %+v, want approved->ship, rejected->failed", g.Transitions)
	}
	if g.Human.OnTimeout != "failed" {
		t.Errorf("onTimeout = %q, want failed (synthesized)", g.Human.OnTimeout)
	}
	if c := g.Human.Choices; c == nil || c.Kind != "single" || len(c.Options) != 2 {
		t.Errorf("choices = %+v, want a 2-option single choice", c)
	}
}

// TestGateChoicesMap: the full choices form maps each event to a target and a
// label, and nested subtrees (pipe/gate) resolve as targets.
func TestGateChoicesMap(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const notify = { prompt: "notify" };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "gate-choices",
  model: "mock",
  states: { work, notify, ship },
  flow: pipe(
    branch(work, {
      else: gate("review", {
        prompt: "decide",
        choices: {
          ship: { to: ship, label: "Ship it" },
          hold: pipe(notify, done),
        },
      }),
    }),
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	g := m.State("gate#review")
	if len(g.Transitions) != 2 {
		t.Fatalf("transitions = %+v, want 2", g.Transitions)
	}
	if g.Transitions[0].On != "ship" || g.Transitions[0].To != "ship" {
		t.Errorf("ship edge = %+v", g.Transitions[0])
	}
	if g.Transitions[1].On != "hold" || g.Transitions[1].To != "notify" {
		t.Errorf("hold edge = %+v, want on hold -> notify (pipe entry)", g.Transitions[1])
	}
	opts := g.Human.Choices.Options
	if len(opts) != 2 || opts[0].Label != "Ship it" || opts[1].Label != "hold" {
		t.Errorf("options = %+v, want labels [Ship it, hold]", opts)
	}
}

// TestGateInLoopExhausted: a gate nests inside loop()'s exhausted: target.
func TestGateInLoopExhausted(t *testing.T) {
	src := `
const draft = { prompt: "write" };
const critic = { prompt: "score", output: { score: "number" }, verdict: ({ output }) => output.score >= 8 };
const publish = { write: "out/x.txt", content: "done" };
export default {
  name: "gate-in-loop",
  model: "mock",
  states: { draft, critic, publish },
  flow: pipe(
    loop(draft, {
      judge: critic,
      maxVisits: 3,
      exhausted: gate("escalate", { prompt: "ship anyway?", approve: publish, timeout: "1h" }),
    }),
    publish,
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	critic := m.State("critic")
	if critic.Transitions[2].To != "gate#escalate" {
		t.Errorf("exhausted edge -> %q, want gate#escalate", critic.Transitions[2].To)
	}
	if m.State("gate#escalate") == nil {
		t.Error("gate#escalate not synthesized from loop exhausted")
	}
}

func TestGateErrors(t *testing.T) {
	wrap := func(g string) string {
		return `
const work = { prompt: "produce" };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "gate-err",
  model: "mock",
  states: { work, ship },
  flow: pipe(branch(work, { else: ` + g + ` })),
};`
	}
	cases := map[string]struct{ gate, want string }{
		"missing prompt":       {`gate("g", { approve: ship })`, "needs a prompt"},
		"unknown key":          {`gate("g", { prompt: "x", approve: ship, bogus: 1 })`, `unknown option "bogus"`},
		"approve and choices":  {`gate("g", { prompt: "x", approve: ship, choices: { a: ship } })`, "not both"},
		"onTimeout no timeout": {`gate("g", { prompt: "x", approve: ship, onTimeout: ship })`, "timeout is not"},
		"bad name":             {`gate("g#x", { prompt: "x", approve: ship })`, "valid identifier"},
		"no edges":             {`gate("g", { prompt: "x" })`, "needs approve"},
		"empty choices":        {`gate("g", { prompt: "x", choices: {} })`, "at least one option"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(wrap(tc.gate)))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestGateDuplicateName(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "dup",
  model: "mock",
  states: { work, ship },
  flow: pipe(
    branch(work, {
      approved: gate("g", { prompt: "a", approve: ship }),
      else: gate("g", { prompt: "b", approve: ship }),
    }),
  ),
};`
	_, err := Parse([]byte(src))
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("err = %v, want gate declared-twice", err)
	}
}
