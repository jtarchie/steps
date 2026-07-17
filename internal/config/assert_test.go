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
			name: "pipeline assert with stdout",
			pipeline: `
assert:
  stdout: nope
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`,
			want: "stdout/code are only valid on task/agent step asserts",
		},
		{
			name: "job assert with code",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
  assert:
    code: 0
`,
			want: "stdout/code are only valid on task/agent step asserts",
		},
		{
			name: "step assert with execution",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
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
    assert:
      code: 0
`,
			want: "assert.code is not valid on agent steps",
		},
		{
			name: "assert on a hook step with execution",
			pipeline: `
jobs:
- name: build
  plan:
  - task: work
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
