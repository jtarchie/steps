package pipeline

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
)

// The planner decides which chains the skip index may skip; the run says, in
// its note, which step stopped that. They once answered from two copies of the
// rule and drifted (15ed677): a chain the note called cacheable was one the
// planner never skipped, or the reverse. This crosses the seam — the planner's
// Chain.Unskippable against the note the walk prints — for every kind and
// modifier, so a second copy of the rule cannot come back unnoticed.
func TestThePlannerAndTheRunAgreeOnWhatIsUncacheable(t *testing.T) {
	cfg, err := config.Parse([]byte(`
resource_types:
- name: sink
  config:
    out: "echo '{\"v\": \"1\"}'"
resources:
- name: out
  type: sink
  source: {}
agents:
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash }
  system: You fix things.
tasks:
- name: inherited
  run: "true"
  fix: fixer
jobs:
- name: j
  plan:
  - task: t
    run: "true"
`), "agree", config.Bundle{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	task := func() config.Step { return config.Step{Task: "t", Run: "true"} }
	with := func(step config.Step, modify func(*config.Step)) config.Step {
		modify(&step)

		return step
	}
	when := func(s *config.Step) { s.When = &config.WhenSpec{Run: "true"} }
	to := func(s *config.Step) { s.To = map[string]string{"success": "next"} }
	across := func(s *config.Step) { s.Across = []config.AcrossVar{{Var: "v", Values: []string{"a", "b"}}} }
	from := func(s *config.Step) {
		s.Context = &config.ContextSpec{From: map[string]config.FromLevel{"earlier": "verdict"}}
	}
	put := config.Step{Put: "out"}
	agent := config.Step{Agent: "fixer"}
	tryTask := config.Step{Try: new(task())}

	blocks := map[string]func(...config.Step) config.Step{
		"do":          func(s ...config.Step) config.Step { return config.Step{Do: s} },
		"in_parallel": func(s ...config.Step) config.Step { return config.Step{InParallel: &config.InParallel{Steps: s}} },
		"race":        func(s ...config.Step) config.Step { return config.Step{Race: &config.Race{Steps: s}} },
	}

	cases := map[string]agreeCase{
		"task":                 {step: task()},
		"task when":            {step: with(task(), when), want: "when: guard"},
		"task to":              {step: with(task(), to), want: "to: routing"},
		"task context from":    {step: with(task(), from), want: "context: from"},
		"task across":          {step: with(task(), across), want: "across block"},
		"task fix":             {step: with(task(), func(s *config.Step) { s.Fix = &config.FixSpec{Agent: "fixer"} }), want: "fix: agent"},
		"task inherited fix":   {step: config.Step{Task: "inherited"}, want: "fix: agent"},
		"put":                  {step: put, want: "put step"},
		"put when":             {step: with(put, when), want: "put step"},
		"agent":                {step: agent, want: "agent step"},
		"agent context from":   {step: with(agent, from), want: "agent step"},
		"try":                  {step: tryTask, want: "try step"},
		"try when":             {step: with(tryTask, when), want: "try step"},
		"load_var":             {step: config.Step{LoadVar: "v", VarFile: "out/v"}, want: "load_var step"},
		"load_var when":        {step: with(config.Step{LoadVar: "v", VarFile: "out/v"}, when), want: "when: guard"},
		"approval":             {step: config.Step{Approval: &config.Approval{Message: "ok?"}}, want: "approval step"},
		"ensemble":             {step: config.Step{Ensemble: &config.Ensemble{Agents: []config.Step{agent, agent}}}, want: "ensemble block: agent step"},
		"nested do in race":    {step: blocks["race"](blocks["do"](task(), task())), want: ""},
		"nested agent":         {step: blocks["do"](blocks["do"](agent)), want: "do block: do block: agent step"},
		"do of tasks across":   {step: with(blocks["do"](task()), across), want: "across block"},
		"in_parallel with fix": {step: blocks["in_parallel"](task(), config.Step{Task: "inherited"}), want: "in_parallel block: fix: agent"},
	}

	for kind, block := range blocks {
		cases[kind+" of tasks"] = agreeCase{step: block(task(), task())}
		cases[kind+" when"] = agreeCase{step: with(block(task()), when), want: "when: guard"}
		cases[kind+" to"] = agreeCase{step: with(block(task()), to), want: "to: routing"}

		// No get: the loader refuses one inside a block, and the planner cannot hash it.
		for name, branch := range map[string]struct {
			step config.Step
			why  string
		}{
			"put":          {step: put, why: "put step"},
			"agent":        {step: agent, why: "agent step"},
			"try":          {step: tryTask, why: "try step"},
			"guarded task": {step: with(task(), when), why: "when: guard"},
			"routed task":  {step: with(task(), to), why: "to: routing"},
			"across task":  {step: with(task(), across), why: "across block"},
			"load_var":     {step: config.Step{LoadVar: "v", VarFile: "out/v"}, why: "load_var step"},
			"approval":     {step: config.Step{Approval: &config.Approval{Message: "ok?"}}, why: "approval step"},
		} {
			cases[kind+" with a "+name] = agreeCase{step: block(task(), branch.step), want: kind + " block: " + branch.why}
		}
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) { assertPlannerAndRunAgree(t, cfg, test) })
	}
}

type agreeCase struct {
	step config.Step
	want string
}

func assertPlannerAndRunAgree(t *testing.T, cfg *config.Config, test agreeCase) {
	t.Helper()

	chains, err := merkle.PlanChains(t.Context(), cfg, "j", []config.Step{test.step}, nil, nil, nil)
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(chains))
	}

	var unskippable bool

	out := captureStdout(t, func() {
		unskippable, err = foldStepUnskippable(t.Context(), cfg, test.step, false)
	})
	if err != nil {
		t.Fatalf("foldStepUnskippable: %v", err)
	}

	if planned := chains[0].Unskippable; planned != unskippable {
		t.Fatalf("planner unskippable = %v, run unskippable = %v (note %q)", planned, unskippable, out)
	}

	want := ""
	if test.want != "" {
		want = "makes this chain uncacheable (" + test.want + ")"
	}

	if unskippable != (want != "") || !strings.Contains(out, want) || (want == "" && out != "") {
		t.Errorf("unskippable = %v, note %q; want reason %q", unskippable, out, test.want)
	}
}
