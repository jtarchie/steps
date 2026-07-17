package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestTaskAssertCodeClearsFailure verifies a task whose non-zero exit matches
// assert.code is a success, and fires on_success (not on_failure).
func TestTaskAssertCodeClearsFailure(t *testing.T) {
	dir := t.TempDir()
	onSuccess := filepath.Join(dir, "on_success.txt")
	onFailure := filepath.Join(dir, "on_failure.txt")
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: exit 3
    assert:
      code: 3
    on_success:
      task: ok
      run: echo ok >> %s
    on_failure:
      task: bad
      run: echo bad >> %s
`, onSuccess, onFailure))

	mustRun(t, path)

	assertLineCount(t, onSuccess, 1)
	assertLineCount(t, onFailure, 0)
}

// TestTaskAssertStdoutMismatchFails verifies a task whose output doesn't match
// assert.stdout fails, even on a zero exit, and fires on_failure.
func TestTaskAssertStdoutMismatchFails(t *testing.T) {
	dir := t.TempDir()
	onFailure := filepath.Join(dir, "on_failure.txt")
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: echo actual-output
    assert:
      stdout: expected-output
    on_failure:
      task: bad
      run: echo bad >> %s
`, onFailure))

	wantRunError(t, path)
	assertLineCount(t, onFailure, 1)
}

// TestJobAssertClearsFailure verifies that a matching job assert.execution
// makes a job containing a failing task exit green.
func TestJobAssertClearsFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    run: exit 1
  on_failure:
    task: notify
    run: echo notified
  assert:
    execution: [work, notify]
`)

	mustRun(t, path)
}

// TestJobAssertMismatchFailsGreenJob verifies a job whose recorded execution
// doesn't match its assert fails, even when the plan itself succeeded.
func TestJobAssertMismatchFailsGreenJob(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
  assert:
    execution: [work, something-that-never-ran]
`)

	wantRunError(t, path)
}

// TestStepsTestFixturePasses runs the shipped self-verifying fixture through
// the `test` subcommand end-to-end.
func TestStepsTestFixturePasses(t *testing.T) {
	err := run([]string{"test", filepath.Join("examples", "hooks.yml")})
	if err != nil {
		t.Fatalf("steps test examples/hooks.yml: %v", err)
	}
}

// TestStepsTestDetectsWrongAssert verifies `steps test` fails when a fixture's
// assert.execution names something that didn't run.
func TestStepsTestDetectsWrongAssert(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, `
assert:
  execution: [build]

jobs:
- name: build
  plan:
  - task: work
    run: "true"
  assert:
    execution: [work, wrong-name]
`)

	err := run([]string{"test", path})
	if err == nil {
		t.Fatal("steps test succeeded, want a failure for the wrong assert.execution")
	}
}

// TestStepsTestPipelineAssertMismatch verifies the top-level assert.execution
// (job names) is enforced by `steps test`.
func TestStepsTestPipelineAssertMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, `
assert:
  execution: [only-this-job]

jobs:
- name: build
  plan:
  - task: work
    run: "true"
`)

	err := run([]string{"test", path})
	if err == nil {
		t.Fatal("steps test succeeded, want a pipeline assert.execution mismatch")
	}

	// Sanity: the state dir the test run created lives beside the pipeline.
	_ = os.RemoveAll(filepath.Join(dir, ".steps"))
}
