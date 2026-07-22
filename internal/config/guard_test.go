package config

import "testing"

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
