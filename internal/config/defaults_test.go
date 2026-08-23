package config

import "testing"

// Naming the same connection string on every agent is pure repetition, and it
// made examples unrunnable for anyone whose setup differed from the author's.
func TestDefaultsModelFillsUnsetAgents(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  model: lmstudio/qwen
agents:
- name: reviewer
- name: big-thinker
  source: { model: openai/gpt-5 }
jobs:
- name: j
  plan:
  - agent: reviewer
    messages:
      - review
  - agent: big-thinker
    messages:
      - think
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	reviewer, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if reviewer.Source.Model != "lmstudio/qwen" {
		t.Errorf("reviewer model = %q, want the default", reviewer.Source.Model)
	}

	// An agent that names its own model keeps it: the default fills gaps, it
	// does not overwrite decisions.
	thinker, err := cfg.FindAgent("big-thinker")
	if err != nil {
		t.Fatal(err)
	}

	if thinker.Source.Model != "openai/gpt-5" {
		t.Errorf("big-thinker model = %q, want its own model preserved", thinker.Source.Model)
	}
}

// $STEPS_MODEL covers the case the pipeline author could not: which model
// exists on the reader's machine.
func TestStepsModelEnvSuppliesTheDefault(t *testing.T) {
	t.Setenv(stepsModelEnv, "lmstudio/from-env")

	path := writeConfig(t, `
agents:
- name: reviewer
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [review] }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if agent.Source.Model != "lmstudio/from-env" {
		t.Errorf("model = %q, want the value from $%s", agent.Source.Model, stepsModelEnv)
	}
}

// $STEPS_MODEL overrides defaults: — a checked-in default cannot know what
// the reader runs, which is exactly why a shipped example with a hardcoded
// model is unrunnable by anyone but its author.
func TestEnvOverridesDefaultsModel(t *testing.T) {
	t.Setenv(stepsModelEnv, "lmstudio/from-env")

	path := writeConfig(t, `
defaults:
  model: lmstudio/from-file
agents:
- name: reviewer
- name: pinned
  source: { model: openai/gpt-5 }
jobs:
- name: j
  plan:
  - agent: reviewer
    messages:
      - review
  - agent: pinned
    messages:
      - think
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if agent.Source.Model != "lmstudio/from-env" {
		t.Errorf("model = %q, want $%s to override defaults.model", agent.Source.Model, stepsModelEnv)
	}

	// An agent naming its own model still wins over both: that is a
	// deliberate per-agent decision, not a gap to fill.
	pinned, err := cfg.FindAgent("pinned")
	if err != nil {
		t.Fatal(err)
	}

	if pinned.Source.Model != "openai/gpt-5" {
		t.Errorf("pinned model = %q, want the agent's own model to win", pinned.Source.Model)
	}
}

// A built-in becomes usable with no agents: entry at all — the defaults apply
// after registration.
func TestDefaultsModelReachesBuiltinAgents(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  model: lmstudio/qwen
jobs:
- name: j
  plan: [{ agent: "@builtin/reviewer", messages: [review] }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent, err := cfg.FindAgent("@builtin/reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if agent.Source.Model != "lmstudio/qwen" {
		t.Errorf("model = %q, want the default to reach a built-in", agent.Source.Model)
	}
}

// An agent with no model anywhere fails at LOAD, naming the three places a
// model can come from. It used to reach planning and fail with `model "" has
// no known provider prefix; set source.endpoint`, which names the wrong fix.
func TestAgentWithNoModelIsRejected(t *testing.T) {
	t.Setenv(stepsModelEnv, "")

	path := writeConfig(t, `
agents:
- name: reviewer
jobs:
- name: j
  plan: [{ agent: reviewer, messages: [review] }]
`)

	wantLoadError(t, path, "has no model; set source.model on it, a pipeline-level defaults.model, or $STEPS_MODEL")
}

// An unreferenced agent needs no model: every built-in profile is always
// registered, and requiring one from each would break every pipeline that
// happens to use none of them.
func TestUnreferencedAgentNeedsNoModel(t *testing.T) {
	t.Setenv(stepsModelEnv, "")

	path := writeConfig(t, `
agents:
- name: never-used
jobs:
- name: j
  plan: [{ task: t, run: "true" }]
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("an unreferenced agent should not require a model: %v", err)
	}
}
