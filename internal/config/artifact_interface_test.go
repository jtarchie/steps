package config

import "testing"

// loadOK loads pipeline and fails on any error.
func loadOK(t *testing.T, pipeline string) *Config {
	t.Helper()

	cfg, err := LoadConfig(writeConfig(t, pipeline))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	return cfg
}

// TestInputsOptionalDefaultsEmpty confirms inputs: is optional: an absent
// declaration defaults to empty and loads cleanly (there is no requirement to
// declare it), for task and filesystem-capable agent steps alike.
func TestInputsOptionalDefaultsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("task step without inputs loads", func(t *testing.T) {
		t.Parallel()

		loadOK(t, `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`)
	})

	t.Run("agent with filesystem tools but no inputs loads", func(t *testing.T) {
		t.Parallel()

		loadOK(t, `
agents:
- name: r
  source: { model: lmstudio/qwen }
  tools: [read_file]
jobs:
- name: j
  plan:
  - agent: r
    prompt: x
`)
	})

	t.Run("inputs: [] loads the same as an absent declaration", func(t *testing.T) {
		t.Parallel()

		loadOK(t, `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
    inputs: []
`)
	})

	t.Run("a declared input naming no producer is still caught at plan time", func(t *testing.T) {
		t.Parallel()

		// This loads (LoadConfig doesn't run flow validation), but
		// ValidateArtifactFlow (exercised in internal/workspace) rejects it.
		loadOK(t, `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    inputs: [repo]
`)
	})
}

func TestGetResourceAlias(t *testing.T) {
	t.Parallel()

	t.Run("resource: aliases the fetched resource", func(t *testing.T) {
		t.Parallel()

		cfg := loadOK(t, `
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"
resources:
- name: repo
  type: dummy
  source: {}
jobs:
- name: j
  plan:
  - get: source
    resource: repo
`)

		step := cfg.Jobs[0].Plan[0]
		if step.Get != "source" || step.GetResourceName() != "repo" {
			t.Errorf("get=%q resource=%q, want get=source resource=repo", step.Get, step.GetResourceName())
		}
	})

	t.Run("resource: on a non-get step errors", func(t *testing.T) {
		t.Parallel()

		wantLoadError(t, writeConfig(t, `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    inputs: []
    resource: repo
`), "resource: is only valid on get steps")
	})

	t.Run("resource: naming a missing resource errors", func(t *testing.T) {
		t.Parallel()

		wantLoadError(t, writeConfig(t, `
jobs:
- name: j
  plan:
  - get: source
    resource: nope
`), "no resource")
	})

	t.Run("GetResourceName falls back to Get when unaliased", func(t *testing.T) {
		t.Parallel()

		step := Step{Get: "repo"}
		if step.GetResourceName() != "repo" {
			t.Errorf("GetResourceName() = %q, want repo", step.GetResourceName())
		}
	})
}

func TestPutInputsAll(t *testing.T) {
	t.Parallel()

	t.Run("inputs: all parses on a put step", func(t *testing.T) {
		t.Parallel()

		cfg := loadOK(t, `
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    out: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: j
  plan:
  - put: thing
    inputs: all
`)

		step := cfg.Jobs[0].Plan[0]
		if !step.InputsAll() {
			t.Errorf("InputsAll() = false, want true")
		}
	})

	t.Run("inputs: all on a task step errors", func(t *testing.T) {
		t.Parallel()

		wantLoadError(t, writeConfig(t, `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    inputs: all
`), "inputs: all is only valid on put steps")
	})

	t.Run("a bare scalar other than all is rejected", func(t *testing.T) {
		t.Parallel()

		wantLoadError(t, writeConfig(t, `
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    inputs: repo
`), `scalar value must be "all"`)
	})
}

func TestArtifactMappingValidation(t *testing.T) {
	t.Parallel()

	t.Run("mapping on a non-task step errors", func(t *testing.T) {
		t.Parallel()

		wantLoadError(t, writeConfig(t, `
workspace:
  strategy: copy
agents:
- name: r
  source: { model: lmstudio/qwen }
  tools: [read_file]
jobs:
- name: j
  plan:
  - agent: r
    prompt: x
    inputs: [repo]
    input_mapping: { repo: source }
`), "only valid on task steps")
	})

	t.Run("a mapping key that is not a declared input errors", func(t *testing.T) {
		t.Parallel()

		wantLoadError(t, writeConfig(t, `
workspace:
  strategy: copy
jobs:
- name: j
  plan:
  - task: work
    run: "true"
    inputs: [repo]
    input_mapping: { nope: source }
`), `input_mapping key "nope" is not a declared input`)
	})

	t.Run("valid mapping resolves onto ResolvedTask", func(t *testing.T) {
		t.Parallel()

		cfg := loadOK(t, `
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
  - task: build
    input_mapping:  { repo: source }
    output_mapping: { built: bits }
`)

		rt, err := cfg.ResolveTask(cfg.Jobs[0].Plan[0])
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}

		if rt.InputMapping["repo"] != "source" || rt.OutputMapping["built"] != "bits" {
			t.Errorf("mappings not carried onto ResolvedTask: in=%v out=%v", rt.InputMapping, rt.OutputMapping)
		}
	})
}

// TestInputSpecDeclaredDistinguishesEmpty confirms an absent inputs: (nil) is
// distinguishable from an explicit inputs: [] — the load-time requirement
// leans on this.
func TestInputSpecDeclaredDistinguishesEmpty(t *testing.T) {
	t.Parallel()

	absent := Step{Task: "t"}
	if absent.InputsDeclared() {
		t.Error("absent inputs: reported as declared")
	}

	empty := Step{Task: "t", Inputs: Inputs()}
	if !empty.InputsDeclared() {
		t.Error("inputs: [] reported as not declared")
	}

	if empty.InputNames() == nil {
		t.Error("inputs: [] InputNames() is nil, want non-nil empty slice")
	}
}
