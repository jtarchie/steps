package config

import (
	"strings"
	"testing"
)

const fromAgents = `
agents:
- name: critic
  source: { model: lmstudio/qwen }
- name: writer
  source: { model: lmstudio/qwen }
`

// TestContextFromLoadsAndStampsTheObligation proves the demand reaches the
// SENDER: the reader names it, and the sender is the step that has to satisfy
// it, because internal/agent builds the sender's tool set and never sees the
// reader.
func TestContextFromLoadsAndStampsTheObligation(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, fromAgents+`
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
  - agent: writer
    inputs: []
    messages:
      - rewrite it
    context:
      from:
        critic: note
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if !cfg.Jobs[0].Plan[0].NoteRequired {
		t.Error("the demanded sender was not marked as owing a note")
	}

	reader := cfg.Jobs[0].Plan[1]
	if got := reader.ContextFrom()["critic"]; got != FromNote {
		t.Errorf("from[critic] = %q, want %q", got, FromNote)
	}
}

// TestContextFromVerdictLevelDemandsNoNote pins the boundary of the
// obligation: a verdict is always emitted, so asking for one costs the sender
// nothing.
func TestContextFromVerdictLevelDemandsNoNote(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, fromAgents+`
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
  - agent: writer
    inputs: []
    messages:
      - rewrite it
    context:
      from:
        critic: verdict
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Jobs[0].Plan[0].NoteRequired {
		t.Error("asking for a verdict alone made the note required")
	}
}

// TestContextFromReadsBackwardsAlongALoop proves a reader may name a step that
// comes LATER in the plan — the revise loop, where the writer reads the critic
// that routes back to it.
func TestContextFromReadsBackwardsAlongALoop(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, fromAgents+`
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    messages:
      - write it
    context:
      from:
        critic: full
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts:
      - approve
      - revise: writer
    max_visits: 2
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a reader naming a later step should load (that is the loop): %v", err)
	}
}

func TestContextFromValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "names a step that does not exist",
			pipeline: fromAgents + `
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
  - agent: writer
    inputs: []
    messages:
      - rewrite it
    context:
      from:
        critik: note
`,
			want: `names "critik", which is not a step in this job`,
		},
		{
			name: "names a step with no verdicts",
			pipeline: fromAgents + `
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
  - agent: writer
    inputs: []
    messages:
      - rewrite it
    context:
      from:
        critic: note
`,
			want: "declares no verdicts:",
		},
		{
			name: "names itself",
			pipeline: fromAgents + `
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
    context:
      from:
        critic: note
`,
			want: "which is this step",
		},
		{
			name: "unknown level",
			pipeline: fromAgents + `
jobs:
- name: j
  plan:
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts: [approve, revise]
  - agent: writer
    inputs: []
    messages:
      - rewrite it
    context:
      from:
        critic: everything
`,
			want: "unknown level",
		},
		{
			name: "empty from",
			pipeline: fromAgents + `
jobs:
- name: j
  plan:
  - agent: writer
    inputs: []
    messages:
      - rewrite it
    context:
      from: {}
`,
			want: "names no steps",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, tc.pipeline))
			if err == nil {
				t.Fatalf("LoadConfig: no error, want one containing %q", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("LoadConfig error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
