package config

import (
	"path/filepath"
	"testing"
)

// TestExampleAgentsLoadsCleanly guards examples/agents.yml (including the
// judge job's handoff: {tool: true} on writer) against schema drift.
func TestExampleAgentsLoadsCleanly(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(filepath.Join("..", "..", "examples", "agents.yml"))
	if err != nil {
		t.Fatalf("LoadConfig(examples/agents.yml): %v", err)
	}

	for _, job := range cfg.Jobs {
		if job.Name != "judge" {
			continue
		}

		var writer *Step

		for i := range job.Plan {
			if job.Plan[i].Agent == "writer" {
				writer = &job.Plan[i]

				break
			}
		}

		if writer == nil {
			t.Fatal("judge job has no writer agent step")
		}

		if writer.Handoff == nil || !writer.Handoff.Tool {
			t.Errorf("judge job's writer step: Handoff = %+v, want {Tool: true}", writer.Handoff)
		}
	}
}

// TestHandoffScalarAndMappingDecode covers scalar handoff: true (context
// only) and every mapping shape, including context's default-true-in-a-
// mapping rule.
func TestHandoffScalarAndMappingDecode(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    prompt: revise it
    handoff: true
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 3
  - task: done
    inputs: []
    run: "true"
`
	path := writeConfig(t, pipeline)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]
	if step.Handoff == nil || !step.Handoff.Context || step.Handoff.Tool {
		t.Errorf("scalar handoff: true = %+v, want {Context: true, Tool: false}", step.Handoff)
	}
}

func TestHandoffMappingShapes(t *testing.T) {
	t.Parallel()

	// Each field means exactly what it says. Nothing turns itself on: an
	// earlier rule defaulted context to true whenever it wasn't named, so
	// `{ tool: true }` quietly enabled two features — and would have made a
	// note-only mapping demand a to: route that has nothing to do with it.
	cases := []struct {
		name        string
		yaml        string
		wantContext bool
		wantTool    bool
	}{
		{"tool only stays tool only", "{ tool: true }", false, true},
		{"context and tool together", "{ context: true, tool: true }", true, true},
		{"context explicit false, tool true", "{ context: false, tool: true }", false, true},
		{"context explicit true, no tool", "{ context: true }", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pipeline := `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    prompt: revise it
    handoff: ` + tc.yaml + `
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 3
  - task: done
    inputs: []
    run: "true"
`
			path := writeConfig(t, pipeline)

			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			h := cfg.Jobs[0].Plan[0].Handoff
			if h == nil || h.Context != tc.wantContext || h.Tool != tc.wantTool {
				t.Errorf("handoff: %s = %+v, want {Context: %v, Tool: %v}", tc.yaml, h, tc.wantContext, tc.wantTool)
			}
		})
	}
}

func TestHandoffValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "handoff on a task step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    to: { success: b }
  - task: b
    inputs: []
    run: "true"
    handoff: true
`,
			want: "handoff is only valid on agent steps",
		},
		{
			name: "handoff enables nothing (scalar false)",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    prompt: revise it
    handoff: false
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 2
  - task: done
    inputs: []
    run: "true"
`,
			want: "handoff enables nothing",
		},
		{
			name: "handoff enables nothing (mapping all-false)",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    prompt: revise it
    handoff: { context: false }
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 2
  - task: done
    inputs: []
    run: "true"
`,
			want: "handoff enables nothing",
		},
		{
			name: "handoff on a step no to: targets",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    prompt: draft it
    handoff: true
`,
			want: "no to: route in this segment targets step",
		},
		{
			name: "handoff on a hook step",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    on_failure:
      agent: writer
      prompt: notify
      handoff: true
`,
			want: "handoff is not valid on hook steps",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)
			wantLoadError(t, path, tc.want)
		})
	}
}

// TestHandoffTargetedByAnotherStepsToIsValid proves a step is a legal
// handoff: target when a DIFFERENT step's to: names it, even though the
// handoff step declares no to: of its own.
func TestHandoffTargetedByAnotherStepsToIsValid(t *testing.T) {
	t.Parallel()

	// writer (plan[0]) has no to: of its own, but critic's revise: targets it.
	full := `
agents:
- name: critic
  source: { model: lmstudio/qwen }
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    prompt: draft it
    handoff: true
  - agent: critic
    inputs: []
    prompt: judge it
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 3
  - task: done
    inputs: []
    run: "true"
`
	path := writeConfig(t, full)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a step targeted by another step's to: should be a valid handoff: target, got: %v", err)
	}
}

// Both directions on one step, in one key. handoff: and handoff_note: used to
// be separate top-level keys whose names implied they were variants of one
// feature when they point opposite ways — the docs needed a dedicated aside
// to say so. Direction is a field now.
func TestHandoffCarriesBothDirections(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: writer
  source: { model: lmstudio/qwen }
- name: reviewer
  source: { model: lmstudio/qwen }
- name: publisher
  source: { model: lmstudio/qwen }
  tools: [read_file]
jobs:
- name: j
  plan:
  - agent: writer
    prompt: draft
  - agent: reviewer
    prompt: review
    handoff: { tool: true, note: true }
    to: { failure: reviewer }
    max_visits: 2
  - agent: publisher
    prompt: publish
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[1]

	if !step.Handoff.Tool {
		t.Error("Tool = false, want the backward previous_run tool enabled")
	}

	if !step.WantsNote() {
		t.Error("WantsNote() = false, want the forward note enabled")
	}

	if !step.Handoff.Receives() {
		t.Error("Receives() = false, want a step granted tool: to count as receiving")
	}
}

// A note-only handoff writes forward along the plan, so it needs no to: route
// pointing at it — the requirement that applies to the backward half only.
func TestNoteOnlyHandoffNeedsNoRoute(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: planner
  source: { model: lmstudio/qwen }
- name: coder
  source: { model: lmstudio/qwen }
  tools: [read_file]
jobs:
- name: j
  plan:
  - agent: planner
    prompt: plan it
    handoff: { note: true }
  - agent: coder
    prompt: build it
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a note-only handoff must not require a to: route: %v", err)
	}
}

// The backward half still does require one, or the field is dead config.
func TestReceivingHandoffStillRequiresARoute(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: draft
    handoff: { context: true }
`)

	wantLoadError(t, path, "no to: route in this segment targets step")
}
