package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleFlowLoadsCleanly guards examples/flow.yml (hooks, when:, and to:,
// consolidated) against schema drift.
func TestExampleFlowLoadsCleanly(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(filepath.Join("..", "..", "examples", "flow.yml"))
	if err != nil {
		t.Fatalf("LoadConfig(examples/flow.yml): %v", err)
	}
}

// TestHooksParseOnStepAndJob checks that on_* / ensure hooks decode (inline)
// onto both a plan step and a job, and that a hook is itself a full Step.
func TestHooksParseOnStepAndJob(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: dummy
  config:
    check: echo '[]'
    in: "true"

jobs:
- name: build
  plan:
  - task: work
    run: "true"
    on_failure:
      task: notify
      run: echo failed
    ensure:
      task: cleanup
      run: echo done
  on_success:
    task: announce
    run: echo announced
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	job := cfg.Jobs[0]
	if job.Hooks.OnSuccess == nil || job.Hooks.OnSuccess.Task != "announce" {
		t.Errorf("job on_success hook = %+v, want task announce", job.Hooks.OnSuccess)
	}

	step := job.Plan[0]
	if step.Hooks.OnFailure == nil || step.Hooks.OnFailure.Run != "echo failed" {
		t.Errorf("step on_failure hook = %+v, want run 'echo failed'", step.Hooks.OnFailure)
	}

	if step.Hooks.Ensure == nil || step.Hooks.Ensure.Task != "cleanup" {
		t.Errorf("step ensure hook = %+v, want task cleanup", step.Hooks.Ensure)
	}
}

func TestHooksReject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "get in a step hook",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    on_failure:
      get: thing
`,
			want: "get is not valid in a hook",
		},
		{
			name: "get in a nested hook",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    on_failure:
      task: notify
      run: "true"
      ensure:
        get: thing
`,
			want: "get is not valid in a hook",
		},
		{
			name: "unrecognized hook body",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    ensure:
      run: echo orphan
`,
			want: "unrecognized hook step",
		},
		{
			name: "job-level hook with inputs",
			pipeline: `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: work
    run: "true"
  on_failure:
    task: notify
    run: "true"
    inputs: [repo]
`,
			want: "inputs/outputs are not valid on job-level hooks",
		},
		{
			name: "get nested inside a job-level hook's own hook",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
  on_failure:
    task: notify
    run: "true"
    ensure:
      get: thing
`,
			want: "get is not valid in a hook",
		},
		{
			name: "job-level hook's own nested hook with inputs",
			pipeline: `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: work
    run: "true"
  on_failure:
    task: notify
    run: "true"
    ensure:
      task: cleanup
      run: "true"
      inputs: [repo]
`,
			want: "inputs/outputs are not valid on job-level hooks",
		},
		{
			name: "image on a put-kind hook",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    on_success:
      put: thing
      image: alpine
`,
			want: "image is not valid on put steps",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tt.pipeline)
			wantLoadError(t, path, tt.want)
		})
	}
}

// TestUsesImagesDetectsHookImage checks the docker fail-fast trigger fires
// when the only image: in the whole pipeline is on a hook step.
func TestUsesImagesDetectsHookImage(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    on_failure:
      task: notify
      run: "true"
      image: alpine
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if !cfg.UsesImages() {
		t.Error("UsesImages() = false, want true (a hook step sets image:)")
	}
}

// TestHookInputsWithoutWorkspaceRejected checks that a hook declaring inputs:
// with no workspace: block fails at load time, the same as a plan step.
func TestHookInputsWithoutWorkspaceRejected(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    on_success:
      task: notify
      run: "true"
      inputs: [repo]
`)

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "workspace:") {
		t.Fatalf("LoadConfig error = %v, want it to mention workspace:", err)
	}
}
