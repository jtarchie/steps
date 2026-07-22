package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestRunJobIsolatedTaskSeesOnlyDeclaredInputsAndOutputs runs a real
// pipeline end to end with strategy: copy: a task declares inputs:
// [repo]/outputs: [built] and a later task declares inputs: [built] only.
// The later task's assertion (no repo/ visible) fails the run if isolation
// doesn't actually hold.
func TestRunJobIsolatedTaskSeesOnlyDeclaredInputsAndOutputs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := `
workspace:
  strategy: copy

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo hello > file.txt

resources:
- name: repo
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: build
    run: mkdir -p built && echo built > built/output.txt
    inputs: [repo]
    outputs: [built]
  - task: verify
    run: test -f built/output.txt && test ! -e repo
    inputs: [built]
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
}

// TestRunJobIsolatedGetAliasMappingAndPutAll runs a real isolated pipeline
// exercising all three Concourse-parity pieces at once: a get: aliasing its
// resource (artifact "source", resource "repo"), a task whose input_mapping/
// output_mapping feed its declared repo/built names from/to the plan artifacts
// source/bits, and a put with inputs: all whose out: command asserts it sees
// exactly source/ and bits/.
func TestRunJobIsolatedGetAliasMappingAndPutAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := `
workspace:
  strategy: copy

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo hello > file.txt
    out: test -d source && test -d bits && test ! -e repo && test ! -e built

resources:
- name: repo
  type: dummy
  source: {}

tasks:
- name: build
  run: test -f repo/file.txt && mkdir -p built && echo built > built/output.txt
  inputs: [repo]
  outputs: [built]

jobs:
- name: build
  plan:
  - get: source
    resource: repo
  - task: build
    input_mapping:  { repo: source }
    output_mapping: { built: bits }
  - put: repo
    inputs: all
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
}

// TestRunJobIsolatedPutSeesOnlyDeclaredInputs checks a put step's out:
// command only sees the artifacts named in its own inputs: — here, exactly
// one directory (the declared "built"), not the "repo" get also fetched
// earlier in the same build.
func TestRunJobIsolatedPutSeesOnlyDeclaredInputs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := `
workspace:
  strategy: copy

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo hello > file.txt
    out: test $(ls -1 | wc -l) -eq 1 && test -d built

resources:
- name: repo
  type: dummy
  source: {}
- name: results
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: build
    run: mkdir -p built && echo built > built/output.txt
    inputs: [repo]
    outputs: [built]
  - put: results
    inputs: [built]
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
}

// TestRunJobIsolatedUnknownInputFailsAtPlanTimeEvenWithForce checks
// validateArtifactFlow's --force-proof static check (job.go's RunJob runs
// it unconditionally, unlike PlanChains): the task never executes, so its
// counter file is never touched.
func TestRunJobIsolatedUnknownInputFailsAtPlanTimeEvenWithForce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: unit
    run: echo ran >> %s
    inputs: [nonexistent]
`, counter)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = run([]string{"--force", path})
	if err == nil {
		t.Fatal("expected an error for a task input naming nothing fetched or produced earlier")
	}

	assertLineCount(t, counter, 0)
}

// TestRunJobIsolatedMerkleSkipRespectsInputsChange confirms inputs: is
// hash-significant once workspace: is configured: an unchanged rerun skips
// the task, but changing its inputs: list re-runs it even though run:
// itself didn't change.
func TestRunJobIsolatedMerkleSkipRespectsInputsChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	writePipeline := func(inputs string) {
		t.Helper()

		pipeline := fmt.Sprintf(`
workspace:
  strategy: copy

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
- name: build
  plan:
  - get: repo
  - task: unit
    run: echo ran >> %s
%s`, counter, inputs)

		err := os.WriteFile(path, []byte(pipeline), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	writePipeline("    inputs: [repo]\n")
	mustRun(t, path)
	assertLineCount(t, counter, 1)

	// Unchanged rerun: same resolved content (including inputs), skipped.
	mustRun(t, path)
	assertLineCount(t, counter, 1)

	// Changing inputs: alone (run: is identical) must still invalidate the
	// cached hash and re-run the task.
	writePipeline("    inputs: []\n")
	mustRun(t, path)
	assertLineCount(t, counter, 2)
}
