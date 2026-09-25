package config

import (
	"slices"
	"testing"
)

func TestWhenDecodesScalarAndMapping(t *testing.T) {
	t.Parallel()

	pipeline := `
resource_types:
- name: rt
  config: { check: "echo '[]'", out: "true" }
resources:
- name: r
  type: rt
  source: {}
jobs:
- name: j
  plan:
  - task: scalar-form
    inputs: []
    run: "true"
    when: test -f marker
  - task: mapping-form
    inputs: []
    run: "true"
    when:
      run: grep -q x file
  - put: r
    when: test -s findings.txt
`
	path := writeConfig(t, pipeline)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan

	if plan[0].When == nil || plan[0].When.Run != "test -f marker" {
		t.Errorf("scalar form decoded to %+v", plan[0].When)
	}

	if plan[1].When == nil || plan[1].When.Run != "grep -q x file" {
		t.Errorf("mapping form decoded to %+v", plan[1].When)
	}

	if plan[2].When == nil || plan[2].When.Run != "test -s findings.txt" {
		t.Errorf("put when decoded to %+v", plan[2].When)
	}
}

func TestWhenDecodesGuardInputs(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: scalar-form
    run: "true"
    when: test -f marker
  - task: guard-inputs
    run: "true"
    when:
      inputs: [answer]
      run: test -s answer/reply.md
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan

	if plan[0].When == nil || plan[0].When.Inputs != nil {
		t.Errorf("scalar form decoded to %+v", plan[0].When)
	}

	if plan[1].When == nil || !slices.Equal(plan[1].When.Inputs, []string{"answer"}) {
		t.Errorf("guard inputs decoded to %+v", plan[1].When)
	}
}

func TestWhenValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "rejected on a get step",
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
    when: test -f x
`,
			want: "when is not valid on get steps",
		},
		{
			name: "empty command rejected",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    inputs: []
    run: "true"
    when: "   "
`,
			want: "when requires a command",
		},
		{
			name: "rejected on a get step inside a hook path too",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    inputs: []
    run: "true"
    on_failure:
      task: notify
      run: "true"
      when: ""
`,
			want: "when requires a command",
		},
		{
			name: "misspelled inputs key",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    when: {run: "true", input: [a]}
`,
			want: `unknown key "input"`,
		},
		{
			name: "guard inputs as a scalar",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    when: {run: "true", inputs: answer}
`,
			want: "inputs must be a list of artifact names",
		},
		{
			name: "guard inputs without a command",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    when: {inputs: [a]}
`,
			want: "when requires a command (run:)",
		},
		{
			name: "invalid guard input name",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    when: {run: "true", inputs: [../x]}
`,
			want: "step 0 (line 5) when: invalid artifact name",
		},
		{
			name: "duplicate guard input",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    when: {run: "true", inputs: [a, a]}
`,
			want: `duplicate input "a"`,
		},
		{
			name: "guard input repeats a step input",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    inputs: [a]
    run: "true"
    when: {run: "true", inputs: [a]}
`,
			want: `when input "a" is already an input of the step`,
		},
		{
			name: "guard input repeats an input inherited from tasks:",
			pipeline: `
tasks:
- name: shared
  run: "true"
  inputs: [a]
jobs:
- name: j
  plan:
  - task: shared
    when: {run: "true", inputs: [a]}
`,
			want: `when input "a" is already an input of the step`,
		},
		{
			name: "guard input repeats an input_mapping key",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    inputs: [src]
    input_mapping: {src: repo}
    run: "true"
    when: {run: "true", inputs: [src]}
`,
			want: `when input "src" is already an input of the step`,
		},
		{
			name: "guard input repeats a try-wrapped step's input",
			pipeline: `
jobs:
- name: j
  plan:
  - try:
      task: t
      inputs: [a]
      run: "true"
    when: {run: "true", inputs: [a]}
`,
			want: `when input "a" is already an input of the step`,
		},
		{
			name: "guard inputs beside a put's inputs: all",
			pipeline: `
resource_types:
- name: rt
  config: { check: "echo '[]'", out: "true" }
resources:
- name: r
  type: rt
  source: {}
jobs:
- name: j
  plan:
  - put: r
    inputs: all
    when: {run: "true", inputs: [a]}
`,
			want: "when inputs are redundant beside inputs: all",
		},
		{
			name: "a hook guard input with a bad name",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    on_failure:
      task: notify
      run: "true"
      when: {run: "true", inputs: [../x]}
`,
			want: "(on_failure hook) when: invalid artifact name",
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

// TestWhenValidOnHookSteps proves a guard is legal on a hook step — a
// conditional cleanup/notification is a real use.
func TestWhenValidOnHookSteps(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: t
    inputs: []
    run: "true"
    on_failure:
      task: notify
      run: "true"
      when: test -f should-notify
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a when: on a hook step should load fine, got: %v", err)
	}
}

// TestWhenInputMayNameTheStepsOwnOutput is the idempotence guard: "skip if
// answer/ already holds a reply" reads the earlier bytes while the step still
// gets its own fresh output directory.
func TestWhenInputMayNameTheStepsOwnOutput(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: t
    outputs: [answer]
    run: "true"
    when: {run: "test ! -s answer/reply.md", inputs: [answer]}
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a guard input naming the step's own output should load, got: %v", err)
	}
}
