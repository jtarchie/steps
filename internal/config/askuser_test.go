package config

import (
	"strings"
	"testing"
)

// TestAskUserDialsOnlyBindOnTheGrant: the same fence allow: has, for the same
// reason. A step's tools: SELECTS a granted tool and resolveEffectiveTools
// substitutes the agent's own spec, so an answered_by: written there would
// read like a routing decision and bind nothing at all.
func TestAskUserDialsOnlyBindOnTheGrant(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: architect
  source: { model: lmstudio/qwen }
- name: writer
  source: { model: lmstudio/qwen }
  tools: [ask_user]
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    messages:
      - x
    tools:
    - builtin: ask_user
      answered_by: architect
`

	wantLoadError(t, writeConfig(t, pipeline), "bind only where the tool is granted")
}

// TestAskUserDialsRefusedOnOtherTools: answered_by/default/options_required
// describe what happens while a question waits, and nothing but ask_user has
// one to wait on.
func TestAskUserDialsRefusedOnOtherTools(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"another builtin": `
  - builtin: web_fetch
    default: minor`,
		"custom tool": `
  - name: lookup
    run: echo hi
    options_required: true`,
		"sub-agent tool": `
  - agent: architect
    description: a bigger model
    answered_by: architect`,
	}

	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pipeline := `
agents:
- name: architect
  source: { model: lmstudio/qwen }
- name: writer
  source: { model: lmstudio/qwen }
  tools:` + entry + `
jobs:
- name: j
  plan: [{ agent: writer, inputs: [], messages: [x] }]
`

			wantLoadError(t, writeConfig(t, pipeline), "only valid on the ask_user builtin")
		})
	}
}

// TestAskUserResponderMustBeAbleToAnswer covers the three ways answered_by:
// can name something that cannot answer. The last is the cycle fence: a
// responder answers, so it may not itself ask — which makes an escalation
// loop unrepresentable rather than merely detected.
func TestAskUserResponderMustBeAbleToAnswer(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ responder, want string }{
		"unknown agent": {`
- name: writer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: ask_user
    answered_by: nobody`, "answered_by"},
		"cli source": {`
- name: architect
  source: { model: "@claude/sonnet" }
- name: writer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: ask_user
    answered_by: architect`, "cannot answer inside this step's conversation"},
		"responder that asks": {`
- name: architect
  source: { model: lmstudio/qwen }
  tools: [ask_user]
- name: writer
  source: { model: lmstudio/qwen }
  tools:
  - builtin: ask_user
    answered_by: architect`, "a responder answers, it does not ask"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pipeline := "agents:" + tc.responder + `
jobs:
- name: j
  plan: [{ agent: writer, inputs: [], messages: [x] }]
`

			wantLoadError(t, writeConfig(t, pipeline), tc.want)
		})
	}
}

// TestAskUserTimeoutIsAcceptedOnACLIAgent is the guard that had to change.
// checkCLIAgentTools refuses timeout: on a builtin because the CLI runs its
// built-ins itself and the deadline would silently not apply — true of every
// builtin until ask_user, which no CLI runs: the child calls the parent's own
// impl over the bridge, so the deadline binds and refusing it would deny a
// CLI agent the one dial that decides how long a person is waited on.
func TestAskUserTimeoutIsAcceptedOnACLIAgent(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: writer
  source: { model: "@claude/sonnet" }
  tools:
  - builtin: ask_user
    timeout: 5m
jobs:
- name: j
  plan: [{ agent: writer, inputs: [], messages: [x] }]
`

	_, err := LoadConfig(writeConfig(t, pipeline))
	if err != nil {
		t.Fatalf("a cli agent's ask_user timeout was refused: %v", err)
	}

	// And the rule it is an exception to still holds for a native builtin.
	native := strings.Replace(pipeline, "builtin: ask_user", "builtin: read_file", 1)
	wantLoadError(t, writeConfig(t, native), "the cli runs its built-ins itself")
}

// TestMaxQuestionsResolution: step wins over agent, agent over the package
// default, and an explicit 0 survives as "no cap" rather than being read as
// unset — the pointer is what makes that distinction expressible.
func TestMaxQuestionsResolution(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: thrifty
  source: { model: lmstudio/qwen }
  max_questions: 1
  tools: [ask_user]
- name: plain
  source: { model: lmstudio/qwen }
  tools: [ask_user]
jobs:
- name: j
  plan:
  - agent: thrifty
    inputs: []
    messages: [x]
  - agent: thrifty
    inputs: []
    messages: [x]
    max_questions: 7
  - agent: plain
    inputs: []
    messages: [x]
  - agent: plain
    inputs: []
    messages: [x]
    max_questions: 0
`

	cfg, err := LoadConfig(writeConfig(t, pipeline))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	for i, want := range []int{1, 7, defaultMaxQuestions, 0} {
		ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[i])
		if err != nil {
			t.Fatalf("ResolveAgentInvocation(step %d): %v", i, err)
		}

		if ri.MaxQuestions != want {
			t.Errorf("step %d resolved max_questions = %d, want %d", i, ri.MaxQuestions, want)
		}
	}
}

// TestMaxQuestionsPlacement: it bounds ask_user calls, and nothing but an
// agent step can ask.
func TestMaxQuestionsPlacement(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ pipeline, want string }{
		"on a task step": {`
tasks:
- name: unit
  run: "true"
jobs:
- name: j
  plan: [{ task: unit, inputs: [], max_questions: 2 }]
`, "max_questions is only valid on agent steps"},
		"negative on a step": {`
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan: [{ agent: writer, inputs: [], messages: [x], max_questions: -1 }]
`, "max_questions must not be negative"},
		"negative on an agent": {`
agents:
- name: writer
  source: { model: lmstudio/qwen }
  max_questions: -1
jobs:
- name: j
  plan: [{ agent: writer, inputs: [], messages: [x] }]
`, "max_questions must not be negative"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, tc.pipeline), tc.want)
		})
	}
}

// TestAskUserIsNotGrantedByDefault: the ability to interrupt a person is a
// capability a pipeline hands over explicitly, exactly like the ability to
// write a file.
func TestAskUserIsNotGrantedByDefault(t *testing.T) {
	t.Parallel()

	for _, spec := range DefaultAgentToolSpecs() {
		if spec.Builtin == AskUserBuiltinName {
			t.Errorf("%s is in the read-only default grant", AskUserBuiltinName)
		}
	}
}
