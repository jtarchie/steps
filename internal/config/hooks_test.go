package config

import (
	"path/filepath"
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

// TestSelfBuildPipelineLoadsCleanly guards experiments/self-build/pipeline-agent.yml
// against schema drift. It is the one pipeline in the repo that is run by hand
// against a live model, so nothing else would catch a bad tool grant or a
// broken YAML anchor until someone had already spent 20 minutes of tokens on
// it. LoadConfig resolves the &fast/&coding aliases and runs
// validateMCPToolGrants, which is entirely static -- it never dials gopls --
// so this needs no network, no model, and no gopls on PATH.
func TestSelfBuildPipelineLoadsCleanly(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "experiments", "self-build", "pipeline-agent.yml")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", path, err)
	}

	// The reviewer's tool grant is load-bearing: search_files and list_dir were
	// removed deliberately (they drive the open-ended exploration that timed
	// the step out), and re-adding one would silently undo that.
	reviewer, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Fatalf("FindAgent(reviewer): %v", err)
	}

	for _, tool := range reviewer.Tools {
		if tool.Builtin == "search_files" || tool.Builtin == "list_dir" {
			t.Errorf("reviewer regained %q; it reviews a diff, it does not explore the tree", tool.Builtin)
		}
	}
}

// TestSelfBuildHandoffChain pins the forward handoff chain in the self-build
// pipeline: planner hands to coder, coder hands to reviewer. Losing a link
// silently returns each agent to re-researching what its predecessor already
// knew — the exact failure this pipeline exists to avoid.
func TestSelfBuildHandoffChain(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "experiments", "self-build", "pipeline-agent.yml")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", path, err)
	}

	// agent name -> (sender it receives from, whether it writes one itself)
	want := map[string]struct {
		from  string
		sends bool
	}{
		"planner":  {"", true},
		"coder":    {"planner", true},
		"reviewer": {"coder", false},
	}

	for _, step := range cfg.Jobs[0].Plan {
		expected, tracked := want[step.Agent]
		if !tracked {
			continue
		}

		if step.HandoffNoteFrom != expected.from {
			t.Errorf("%s receives a note from %q, want %q", step.Agent, step.HandoffNoteFrom, expected.from)
		}

		if step.WantsNote() != expected.sends {
			t.Errorf("%s writes a handoff note = %v, want %v", step.Agent, step.WantsNote(), expected.sends)
		}
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
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

// TestStepHookInputsWithoutWorkspaceLoad checks that a step-level hook may
// declare inputs: without a workspace: block — a validated contract, not an
// error (unlike a job-level hook, which may not declare artifacts at all).
func TestStepHookInputsWithoutWorkspaceLoad(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
    on_success:
      task: notify
      run: "true"
      inputs: [repo]
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
