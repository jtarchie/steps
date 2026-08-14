package config

import (
	"testing"
)

// TestAssertParses checks that assert directives decode at pipeline, job, and
// step levels.
//
//nolint:cyclop // straight-line field checks across three assert levels
func TestAssertParses(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
assert:
  execution: [build]

jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
    assert:
      stdout: hello
      code: 0
  assert:
    execution: [work]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Assert == nil || len(cfg.Assert.Execution) != 1 || cfg.Assert.Execution[0] != "build" {
		t.Errorf("pipeline assert = %+v, want execution [build]", cfg.Assert)
	}

	job := cfg.Jobs[0]
	if job.Assert == nil || len(job.Assert.Execution) != 1 {
		t.Errorf("job assert = %+v, want execution [work]", job.Assert)
	}

	step := job.Plan[0]
	if step.Assert == nil || step.Assert.Stdout == nil || *step.Assert.Stdout != "hello" || step.Assert.Code == nil {
		t.Errorf("step assert = %+v, want stdout hello + code 0", step.Assert)
	}
}

func TestAssertReject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "assert.verdict on a step that declares none",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: c
    inputs: []
    prompt: x
    assert:
      verdict: approve
`,
			want: "the step declares no verdicts:",
		},
		{
			name: "assert.verdict outside the declared vocabulary",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: c
    inputs: []
    prompt: x
    verdicts: [approve, revise]
    assert:
      verdict: aprove
`,
			want: `assert.verdict "aprove" is not one of the declared verdicts`,
		},
		{
			name: "assert.verdict on a task",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
    assert:
      verdict: approve
`,
			want: "assert.verdict is only valid on agent steps",
		},
		{
			name: "pipeline assert with stdout",
			pipeline: `
assert:
  stdout: nope
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
`,
			want: "stdout/code/verdict/files are only valid on task/agent step asserts",
		},
		{
			name: "job assert with code",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
  assert:
    code: 0
`,
			want: "stdout/code/verdict/files are only valid on task/agent step asserts",
		},
		{
			name: "step assert with execution",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
    assert:
      execution: [work]
`,
			want: "execution is only valid on job/pipeline asserts",
		},
		{
			name: "assert on a get step",
			pipeline: `
jobs:
- name: build
  plan:
  - get: thing
    assert:
      stdout: nope
`,
			want: "assert is not valid on get steps",
		},
		{
			name: "assert on a put step",
			pipeline: `
jobs:
- name: build
  plan:
  - put: thing
    assert:
      code: 0
`,
			want: "assert is not valid on put steps",
		},
		{
			name: "code on an agent step",
			pipeline: `
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    assert:
      code: 0
`,
			want: "assert.code is not valid on agent steps",
		},
		{
			name: "assert.files escapes the artifact via ..",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [answer]
    run: "true"
    assert:
      files: ["../etc/passwd"]
`,
			want: "invalid artifact",
		},
		{
			name: "assert.files names no declared output",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    outputs: [answer]
    run: "true"
    assert:
      files: [reply.md]
`,
			want: `names artifact "reply.md", which is not one of this step's outputs (answer)`,
		},
		{
			name: "assert on a hook step with execution",
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
      assert:
        execution: [notify]
`,
			want: "execution is only valid on job/pipeline asserts",
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

// TestAssertFilesResolvesTaskOutputs proves assert.files checks a path's
// first segment against the step's EFFECTIVE outputs — inherited from a
// named tasks: entry when the step declares none of its own — not just
// step.Outputs read raw. A step that references a task by name and adds no
// outputs: override would otherwise see an empty list and reject every path.
func TestAssertFilesResolvesTaskOutputs(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: draft
  inputs: []
  outputs: [answer]
  run: "true"

jobs:
- name: build
  plan:
  - task: draft
    assert:
      files: [answer/reply.md]
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v, want a step referencing a named task to inherit its outputs: for assert.files", err)
	}
}
