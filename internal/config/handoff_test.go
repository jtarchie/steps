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

		writer := job.Plan[0]
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
    prompt: revise it
    handoff: true
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 3
  - task: done
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

	cases := []struct {
		name        string
		yaml        string
		wantContext bool
		wantTool    bool
	}{
		{"tool only, context defaults true", "{ tool: true }", true, true},
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
    prompt: revise it
    handoff: ` + tc.yaml + `
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 3
  - task: done
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
    run: "true"
    to: { success: b }
  - task: b
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
    prompt: revise it
    handoff: false
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 2
  - task: done
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
    prompt: revise it
    handoff: { context: false }
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 2
  - task: done
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
    prompt: draft it
    handoff: true
  - agent: critic
    prompt: judge it
    verdicts: [approve, revise]
    to: { approve: done, revise: writer }
    max_visits: 3
  - task: done
    run: "true"
`
	path := writeConfig(t, full)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a step targeted by another step's to: should be a valid handoff: target, got: %v", err)
	}
}
