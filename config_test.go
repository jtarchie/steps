package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes pipeline to a temp pipeline.yml and returns its path.
func writeConfig(t *testing.T, pipeline string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// wantLoadError loads path and fails unless it errors with a message
// containing want.
func wantLoadError(t *testing.T, path, want string) {
	t.Helper()

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatalf("LoadConfig(%q): expected an error containing %q, got nil", path, want)
	}

	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("LoadConfig(%q) error = %q, want it to contain %q", path, got, want)
	}
}

func TestConfigValidateWorkspaceStrategy(t *testing.T) {
	t.Parallel()

	t.Run("unknown strategy", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: rsync
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, `workspace.strategy "rsync" must be one of copy, btrfs`)
	})

	t.Run("btrfs without root", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: btrfs
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, "workspace.root is required for strategy: btrfs")
	})

	t.Run("compression on copy strategy errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy
  options:
    compression: zstd
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, "workspace.options.compression is only valid for strategy: btrfs")
	})

	t.Run("invalid compression value", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: btrfs
  root: /mnt/btrfs
  options:
    compression: gzip
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, `workspace.options.compression "gzip" must be one of zstd, lzo, zlib, none`)
	})

	t.Run("valid copy config loads cleanly", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})
}

func TestConfigValidateArtifactDeclsRequireWorkspace(t *testing.T) {
	t.Parallel()

	t.Run("step inputs without a workspace: block errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: [repo]
`)
		wantLoadError(t, path, "inputs/outputs require a top-level workspace: block")
	})

	t.Run("top-level task outputs without a workspace: block errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
tasks:
- name: build
  run: echo hi
  outputs: [built]

jobs:
- name: build
  plan:
  - task: build
`)
		wantLoadError(t, path, `task "build": inputs/outputs require a top-level workspace: block`)
	})
}

func TestConfigValidateArtifactDeclsStepKindRestrictions(t *testing.T) {
	t.Parallel()

	t.Run("inputs on a get step errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: thing
    inputs: [repo]
`)
		wantLoadError(t, path, "inputs/outputs are not valid on get steps")
	})

	t.Run("outputs on a put step errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

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
- name: build
  plan:
  - put: thing
    outputs: [built]
`)
		wantLoadError(t, path, "outputs are not valid on put steps")
	})
}

func TestConfigValidateArtifactNames(t *testing.T) {
	t.Parallel()

	t.Run("invalid name rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: ["../evil"]
`)
		wantLoadError(t, path, "invalid artifact name")
	})

	t.Run("duplicate input name rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: [repo, repo]
`)
		wantLoadError(t, path, `duplicate input "repo"`)
	})

	t.Run("name in both inputs and outputs rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: [repo]
    outputs: [repo]
`)
		wantLoadError(t, path, `"repo" cannot be both an input and an output`)
	})
}

// assertResolvedIO fails the test unless rt's inputs/outputs exactly equal
// wantInputs/wantOutputs.
func assertResolvedIO(t *testing.T, rt resolvedTask, wantInputs, wantOutputs []string) {
	t.Helper()

	if !slicesEqual(rt.inputs, wantInputs) {
		t.Errorf("inputs = %v, want %v", rt.inputs, wantInputs)
	}

	if !slicesEqual(rt.outputs, wantOutputs) {
		t.Errorf("outputs = %v, want %v", rt.outputs, wantOutputs)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestExampleIsolatedLoadsCleanly guards examples/isolated.yml against
// silently drifting out of sync with the workspace: schema/validation rules
// it's meant to demonstrate.
func TestExampleIsolatedLoadsCleanly(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(filepath.Join("examples", "isolated.yml"))
	if err != nil {
		t.Fatalf("LoadConfig(examples/isolated.yml): %v", err)
	}
}

// TestResolveTaskInputsOutputsOverride mirrors
// TestRunJobTaskReferenceStepFixOverridesTaskFix (tasks_test.go): a step's
// own inputs:/outputs:, when set (including an explicit empty list),
// override the referenced task's for that step only.
func TestResolveTaskInputsOutputsOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Workspace: &WorkspaceConfig{Strategy: "copy"},
		Tasks: []Task{
			{Name: "build", Run: "echo hi", Inputs: []string{"repo"}, Outputs: []string{"built"}},
		},
	}

	t.Run("step with no inputs/outputs of its own inherits the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := resolveTask(cfg, Step{Task: "build"})
		if err != nil {
			t.Fatal(err)
		}

		assertResolvedIO(t, rt, []string{"repo"}, []string{"built"})
	})

	t.Run("step's own explicit empty inputs overrides the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := resolveTask(cfg, Step{Task: "build", Inputs: []string{}})
		if err != nil {
			t.Fatal(err)
		}

		assertResolvedIO(t, rt, []string{}, []string{"built"})
	})

	t.Run("step's own outputs overrides the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := resolveTask(cfg, Step{Task: "build", Outputs: []string{"artifact"}})
		if err != nil {
			t.Fatal(err)
		}

		assertResolvedIO(t, rt, []string{"repo"}, []string{"artifact"})
	})
}
