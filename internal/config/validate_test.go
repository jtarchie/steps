package config

import (
	"strings"
	"testing"
)

// A pipeline with several unrelated mistakes reports all of them, so fixing a
// config takes one load per author, not one load per mistake.
func TestValidateReportsEveryError(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: mock
  config:
    check: 'echo ''[{"ref":"1"}]'''
    in: "true"
resources:
- name: repo
  type: mock
  source: {}
jobs:
- name: j
  plan:
  - get: repo
    when: test -f x
  - task: t
    run: "true"
    trigger: true
  - agent: ghost
    prompt: x
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, want := range []string{
		"when is not valid on get steps",
		"trigger is only valid on get steps",
		`no agent named "ghost"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q\n  is missing %q", err.Error(), want)
		}
	}
}

// Validation errors name the step's source line, so the reader doesn't have to
// count plan entries to find the one being complained about.
func TestValidateErrorsCarryLineNumbers(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: first
    run: "true"
  - task: second
    run: "true"
    timeout: soon
`)

	wantLoadError(t, path, "step 1 (line 7)")
}

func TestValidateStepKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			// Previously loaded clean and failed mid-run, after earlier steps
			// had already executed.
			name: "two kinds on one step",
			pipeline: `
agents:
- name: a
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: t
    agent: a
    run: "true"
`,
			want: "step sets task and agent, but a step is exactly one of get/task/put/agent",
		},
		{
			name: "no kind at all",
			pipeline: `
jobs:
- name: j
  plan:
  - run: "true"
`,
			want: "step names no kind, set one of get/task/put/agent",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, test.pipeline), test.want)
		})
	}
}

// A step naming something the pipeline doesn't define used to survive load and
// fail partway through a run, after earlier steps had already done their work.
func TestValidateStepReferences(t *testing.T) {
	t.Parallel()

	resources := `
resource_types:
- name: mock
  config:
    check: 'echo ''[{"ref":"1"}]'''
    in: "true"
    out: "true"
resources:
- name: repo
  type: mock
  source: {}
`

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "agent step names an unknown agent",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan: [{ agent: reviwer, prompt: x }]
`,
			want: `no agent named "reviwer" (did you mean "reviewer"?)`,
		},
		{
			name:     "get step names an unknown resource",
			pipeline: resources + "jobs:\n- name: j\n  plan: [{ get: rep }]\n",
			want:     `no resource named "rep" (did you mean "repo"?)`,
		},
		{
			name:     "put step names an unknown resource",
			pipeline: resources + "jobs:\n- name: j\n  plan: [{ put: relase }]\n",
			want:     `no resource named "relase"`,
		},
		{
			name: "task step names an unknown tasks entry",
			pipeline: `
tasks:
- name: build
  run: make
jobs:
- name: j
  plan: [{ task: biuld }]
`,
			want: `no task named "biuld" (did you mean "build"?)`,
		},
		{
			name: "hook step names an unknown agent",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    on_failure: { agent: ghost, prompt: x }
`,
			want: `no agent named "ghost"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, test.pipeline), test.want)
		})
	}
}

// An inline task carries its own run: and never consults tasks:, so its task:
// label is a name for the step rather than a reference to resolve.
func TestValidateStepReferencesAllowsInlineTask(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: anything
    run: "true"
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

// trigger:, version: and params: used to be accepted and ignored on the wrong
// step kind — the one class of misplaced field that failed silently.
func TestValidateStepFieldPlacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "trigger on a task step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    trigger: true
`,
			want: "trigger is only valid on get steps",
		},
		{
			name: "version on a task step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    version: latest
`,
			want: "version is only valid on get steps",
		},
		{
			name: "params on an agent step",
			pipeline: `
agents:
- name: a
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: a
    prompt: x
    params: { branch: main }
`,
			want: "params is only valid on get and put steps",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, test.pipeline), test.want)
		})
	}
}

// A get step keeps its own fields, so the placement rules don't overreach.
func TestValidateStepFieldPlacementAllowsGetAndPut(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: mock
  config:
    check: 'echo ''[{"ref":"1"}]'''
    in: "true"
    out: "true"
resources:
- name: repo
  type: mock
  source: {}
jobs:
- name: j
  plan:
  - get: repo
    trigger: true
    version: latest
  - put: repo
    params: { ref: main }
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

// A misspelled name points at the one that was meant, instead of only
// reporting that it wasn't found.
func TestFindSuggestsNearestName(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks:         []Task{{Name: "build"}},
		Agents:        []Agent{{Name: "reviewer"}},
		Resources:     []Resource{{Name: "repo"}},
		ResourceTypes: []ResourceType{{Name: "github"}},
		MCPServers:    []MCPServer{{Name: "docs"}},
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "task", err: second(cfg.FindTask("biuld")), want: `no task named "biuld" (did you mean "build"?) (available: [build])`},
		{name: "agent", err: second(cfg.FindAgent("reviewerr")), want: `no agent named "reviewerr" (did you mean "reviewer"?)`},
		{name: "resource", err: second(cfg.FindResource("rep")), want: `no resource named "rep" (did you mean "repo"?)`},
		{name: "resource_type", err: second(cfg.FindResourceType("gihub")), want: `no resource_type named "gihub" (did you mean "github"?)`},
		{name: "mcp server", err: second(cfg.FindMCPServer("doc")), want: `no mcp_servers entry named "doc" (did you mean "docs"?)`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.err == nil {
				t.Fatal("expected an error")
			}

			if got := test.err.Error(); !strings.Contains(got, test.want) {
				t.Errorf("error = %q, want it to contain %q", got, test.want)
			}
		})
	}
}

// A name nowhere near an existing one still lists the candidates, without
// guessing at a match that isn't there.
func TestFindWithoutNearMatchStillListsCandidates(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{{Name: "build"}}}

	_, err := cfg.FindTask("deploy")
	if err == nil {
		t.Fatal("expected an error")
	}

	got := err.Error()
	if strings.Contains(got, "did you mean") {
		t.Errorf("error = %q, want no suggestion for an unrelated name", got)
	}

	if !strings.Contains(got, "available: [build]") {
		t.Errorf("error = %q, want it to list the available names", got)
	}
}

// second returns the error from a (value, error) pair, discarding the value.
func second[T any](_ T, err error) error { return err }

// A tasks: entry and a step now spell inputs: the same way — one type, one
// meaning in the schema, rather than two that happened to share a name.
func TestTaskInputsAcceptsTheSameFormsAsAStep(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
workspace:
  strategy: copy
tasks:
- name: build
  run: "true"
  inputs: [repo]
  outputs: [built]
jobs:
- name: j
  plan:
  - task: fetch
    run: "true"
    outputs: [repo]
  - task: build
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	task, err := cfg.FindTask("build")
	if err != nil {
		t.Fatal(err)
	}

	if got := task.Inputs.Names; len(got) != 1 || got[0] != "repo" {
		t.Errorf("task inputs = %v, want [repo]", got)
	}
}

// `inputs: all` stays put-only. On a reusable task it would resolve to
// whatever the calling job happened to have, which is the opposite of
// reusable.
func TestTaskInputsAllIsRejected(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: build
  run: "true"
  inputs: all
jobs:
- name: j
  plan: [{ task: build }]
`)

	wantLoadError(t, path, `task "build": inputs: all is only valid on put steps`)
}
