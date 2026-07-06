package machine

import (
	"strings"
	"testing"
)

func evidenceMachine(state string) string {
	return `
const work = ` + state + `;
export default {
  name: "evidence",
  input: { article: "string" },
  model: "mock",
  states: { work },
  flow: pipe(work, done),
};`
}

// TestEvidenceComposition: the prompt renders as instruction + labeled
// blocks in declaration order, with falsy blocks omitted.
func TestEvidenceComposition(t *testing.T) {
	m, err := Parse([]byte(evidenceMachine(`{
  prompt: "Summarize the article in 150 words.",
  evidence: {
    article: true,
    reviewer_feedback: ({ work }) => work && "fix everything",
  },
  output: { summary: "string" },
}`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := m.State("work").Agent

	// First visit: no prior work output -> feedback block omitted.
	got, err := a.Prompt.String(map[string]any{"article": "THE ARTICLE"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "Summarize the article in 150 words.\n\nARTICLE:\nTHE ARTICLE"
	if got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}

	// Revisit: the feedback block appears, underscore key renders spaced.
	got, err = a.Prompt.String(map[string]any{"article": "THE ARTICLE", "work": map[string]any{"summary": "s"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "REVIEWER FEEDBACK:\nfix everything") {
		t.Errorf("prompt = %q, want the REVIEWER FEEDBACK block", got)
	}
}

// TestEvidenceStructuredValue: non-string block values render as YAML.
func TestEvidenceStructuredValue(t *testing.T) {
	m, err := Parse([]byte(evidenceMachine(`{
  prompt: "Judge these.",
  evidence: { findings: ({ article }) => [{ where: article, why: "bad" }] },
}`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := m.State("work").Agent.Prompt.String(map[string]any{"article": "x.go"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "FINDINGS:\n") || !strings.Contains(got, "where: x.go") {
		t.Errorf("prompt = %q, want a YAML FINDINGS block", got)
	}
}

func TestEvidenceErrors(t *testing.T) {
	cases := map[string]struct{ state, want string }{
		"no prompt": {
			`{ evidence: { article: true } }`,
			"evidence needs a prompt",
		},
		"unknown true key": {
			`{ prompt: "x", evidence: { no_such_key: true } }`,
			"names no run input",
		},
		"bad value type": {
			`{ prompt: "x", evidence: { article: 5 } }`,
			"must be true",
		},
		"unknown field in block fn": {
			`{ prompt: "x", evidence: { a: ({ no_such_field }) => no_such_field } }`,
			"no_such_field",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(evidenceMachine(tc.state)))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestLoopEscalateShorthand: escalate: synthesizes gate#<judge>_escalate with
// approve rejoining the loop's then route and timeout -> failed.
func TestLoopEscalateShorthand(t *testing.T) {
	src := `
const work = { prompt: "produce" };
const judge = { prompt: "score", output: { score: "number" }, verdict: ({ output }) => output.score >= 8 };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "escalate",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(
    loop(work, {
      judge: judge,
      maxVisits: 3,
      escalate: { prompt: ({ judge }) => "ship at score " + judge.score + "?", timeout: "1h" },
    }),
    ship,
  ),
};`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	j := m.State("judge")
	if j.Transitions[2].To != "gate#judge_escalate" {
		t.Errorf("exhausted -> %q, want gate#judge_escalate", j.Transitions[2].To)
	}
	g := m.State("gate#judge_escalate")
	if g == nil || !g.Gate {
		t.Fatal("gate#judge_escalate not synthesized")
	}
	if g.Transitions[0].On != "approved" || g.Transitions[0].To != "ship" {
		t.Errorf("approve edge = %+v, want approved -> ship (the loop's then)", g.Transitions[0])
	}
	if g.Transitions[1].To != "failed" || g.Human.OnTimeout != "failed" {
		t.Errorf("reject/timeout = %+v / %q, want failed", g.Transitions[1], g.Human.OnTimeout)
	}
}

func TestLoopEscalateErrors(t *testing.T) {
	wrap := func(opts string) string {
		return `
const work = { prompt: "produce" };
const judge = { prompt: "score", verdict: ({ output }) => true };
const ship = { write: "out/x.txt", content: "done" };
export default {
  name: "esc-err",
  model: "mock",
  states: { work, judge, ship },
  flow: pipe(loop(work, { judge: judge, maxVisits: 3, ` + opts + ` }), ship),
};`
	}
	cases := map[string]struct{ opts, want string }{
		"both routes": {`escalate: "ship?", exhausted: fail`, "not both"},
		"unknown key": {`escalate: { prompt: "x", approve: ship }`, `unknown key "approve"`},
		"no prompt":   {`escalate: { timeout: "1h" }`, "needs a prompt"},
		"bad type":    {`escalate: 5`, "must be a prompt"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(wrap(tc.opts)))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}
