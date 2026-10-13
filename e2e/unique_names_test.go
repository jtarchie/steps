package e2e

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// Two top-level entries of one kind under one name is refused at load, as
// Concourse's set-pipeline does: every Find* returns the first match, so the
// second definition would silently never run, and the store keys history by
// name, so the two would share one.
func TestValidateRefusesDuplicateTopLevelNames(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-used-for-any-call")

	tests := []struct {
		name     string
		pipeline string
		want     []string
	}{
		{
			name: "jobs",
			pipeline: `
jobs:
- name: build
  plan: [{ task: a, run: "true" }]
- name: test
  plan: [{ task: b, run: "true" }]
- name: build
  plan: [{ task: c, run: "true" }]
`,
			want: []string{`jobs[0] (line 3) and jobs[2] (line 7) have the same name "build"`},
		},
		{
			name: "resources",
			pipeline: `
resources:
- name: tick
  type: cron
  source: { expression: "* * * * *" }
- name: tick
  type: cron
  source: { expression: "*/5 * * * *" }
jobs:
- name: j
  plan: [{ get: tick }]
`,
			want: []string{`resources[0] (line 3) and resources[1] (line 6) have the same name "tick"`},
		},
		{
			name: "resource_types",
			pipeline: `
resource_types:
- name: thing
  config: { check: "true", in: "true" }
- name: thing
  config: { check: echo, in: "true" }
jobs:
- name: j
  plan: [{ task: a, run: "true" }]
`,
			want: []string{`resource_types[0] (line 3) and resource_types[1] (line 5) have the same name "thing"`},
		},
		{
			name: "resource_types shadowing a built-in twice",
			pipeline: `
resource_types:
- name: git
  config: { check: "true", in: "true" }
- name: git
  config: { check: echo, in: "true" }
jobs:
- name: j
  plan: [{ task: a, run: "true" }]
`,
			want: []string{`resource_types[0] (line 3) and resource_types[1] (line 5) have the same name "git"`},
		},
		{
			name: "tasks",
			pipeline: `
tasks:
- name: greet
  run: echo hi
- name: greet
  run: echo bye
jobs:
- name: j
  plan: [{ task: greet }]
`,
			want: []string{`tasks[0] (line 3) and tasks[1] (line 5) have the same name "greet"`},
		},
		{
			name: "agents",
			pipeline: `
agents:
- name: helper
  source: { model: openrouter/qwen/qwen3.7-flash }
- name: helper
  source: { model: openrouter/anthropic/claude-sonnet-4.5 }
jobs:
- name: j
  plan: [{ agent: helper, messages: [hi] }]
`,
			want: []string{`agents[0] (line 3) and agents[1] (line 5) have the same name "helper"`},
		},
		{
			name: "every kind reported in one pass",
			pipeline: `
tasks:
- name: t
  run: "true"
- name: t
  run: "true"
jobs:
- name: j
  plan: [{ task: t }]
- name: j
  plan: [{ task: t }]
`,
			want: []string{
				`tasks[0] (line 3) and tasks[1] (line 5) have the same name "t"`,
				`jobs[0] (line 8) and jobs[1] (line 10) have the same name "j"`,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writePipeline(t, t.TempDir(), tc.pipeline)

			err := cli.Run([]string{"validate", path})
			if err == nil {
				t.Fatal("expected validate to refuse a duplicated name")
			}

			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q\n  is missing %q", err.Error(), want)
				}
			}
		})
	}
}

// Declaring a built-in's name is how a pipeline replaces a built-in resource
// type or configures a built-in agent, so the uniqueness check must see only
// what the pipeline itself declared — never the entries registration adds.
func TestValidateAcceptsAUserDefinitionNamedLikeABuiltin(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key-not-used-for-any-call")

	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: git
  config: { check: echo mine, in: "true", out: "true" }
agents:
- name: "@builtin/reviewer"
  source: { model: openrouter/qwen/qwen3.7-flash }
resources:
- name: repo
  type: git
  source: {}
jobs:
- name: j
  plan:
  - get: repo
  - agent: "@builtin/reviewer"
    messages: [hi]
`)

	err := cli.Run([]string{"validate", path})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
}
