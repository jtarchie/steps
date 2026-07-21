package config

import "testing"

func TestTransitionsLoadAndDecode(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: critic
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: draft
    run: "true"
  - agent: critic
    prompt: judge it
    verdicts: [approve, revise, escalate]
    to:
      approve: publish
      revise: draft
      escalate: publish
      failure: publish
    max_visits: 3
  - task: publish
    run: "true"
`
	path := writeConfig(t, pipeline)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	critic := cfg.Jobs[0].Plan[1]
	if critic.To["revise"] != "draft" {
		t.Errorf("to[revise] = %q, want draft", critic.To["revise"])
	}

	if critic.MaxVisits != 3 {
		t.Errorf("max_visits = %d, want 3", critic.MaxVisits)
	}

	if len(critic.Verdicts) != 3 {
		t.Errorf("verdicts = %v, want 3 entries", critic.Verdicts)
	}
}

func TestTransitionsBinaryLoads(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    to:
      success: publish
      failure: work
    max_visits: 2
  - task: publish
    run: "true"
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a binary success/failure loop should load, got: %v", err)
	}
}

func TestTransitionsValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "unresolvable target",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    to: { success: nowhere }
`,
			want: "not a step in the same segment",
		},
		{
			name: "backward without max_visits",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    run: "true"
  - task: b
    run: "true"
    to: { failure: a }
`,
			want: "max_visits must be set",
		},
		{
			name: "backward max_visits exceeds ceiling",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    run: "true"
  - task: b
    run: "true"
    to: { failure: a }
    max_visits: 1001
`,
			want: "exceeds the maximum",
		},
		{
			name: "to on a get step",
			pipeline: `
resource_types:
- name: rt
  config: { check: "echo '[]'", in: "true" }
resources:
- name: r
  type: rt
  source: {}
jobs:
- name: j
  plan:
  - get: r
    to: { success: r }
`,
			want: "not valid on get steps",
		},
		{
			name: "to on a hook step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    on_failure:
      task: notify
      run: "true"
      to: { success: notify }
`,
			want: "not valid on hook steps",
		},
		{
			name: "unknown binary key",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    to: { done: publish }
  - task: publish
    run: "true"
`,
			want: `key "done" is not valid`,
		},
		{
			name: "duplicate names in to-using segment",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    to: { success: work }
  - task: work
    run: "true"
`,
			want: "duplicated within a to:-using segment",
		},
		{
			name: "verdicts on a task",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    verdicts: [a, b]
    to: { a: work, b: work }
    max_visits: 2
`,
			want: "verdicts is only valid on agent steps",
		},
		{
			name: "verdict collides with reserved key",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    prompt: x
    verdicts: [success]
    to: { success: c }
`,
			want: "collides with a reserved key",
		},
		{
			name: "declared verdict not routed",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    prompt: x
    verdicts: [approve, revise]
    to: { approve: done }
  - task: done
    run: "true"
`,
			want: `verdict "revise" has no to: target`,
		},
		{
			name: "to key not a declared verdict",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    prompt: x
    verdicts: [approve]
    to: { approve: done, bogus: done }
  - task: done
    run: "true"
`,
			want: `key "bogus" is not a declared verdict`,
		},
		{
			name: "success key in verdict mode",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    prompt: x
    verdicts: [approve]
    to: { approve: done, success: done }
  - task: done
    run: "true"
`,
			want: "success is not valid in verdict mode",
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

// TestTransitionsDuplicateNamesAllowedWithoutTo proves the uniqueness check is
// gated on to: usage — a job with duplicate step names but no routing loads.
func TestTransitionsDuplicateNamesAllowedWithoutTo(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
  - task: work
    run: "true"
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("duplicate names in a to:-free job should load, got: %v", err)
	}
}

// TestTransitionsForwardNeedsNoMaxVisits proves an all-forward to: is legal
// without max_visits.
func TestTransitionsForwardNeedsNoMaxVisits(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: a
    run: "true"
    to: { success: c }
  - task: b
    run: "true"
  - task: c
    run: "true"
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a forward-only to: should load without max_visits, got: %v", err)
	}
}
