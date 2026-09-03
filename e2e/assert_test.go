package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
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
    inputs: []
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
    inputs: []
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
    inputs: []
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
    inputs: []
    run: "true"
  assert:
    execution: [work, something-that-never-ran]
`)

	wantRunError(t, path)
}

// TestJobAssertOutcomeFailedRequiresAFailure is the discriminating half of
// assert:. The two jobs run the SAME steps and differ only in whether the plan
// concluded failure — which is precisely what assert.execution cannot see, and
// then erases by clearing the error on a match.
func TestJobAssertOutcomeFailedRequiresAFailure(t *testing.T) {
	tests := []struct {
		name    string
		run     string
		wantErr bool
	}{
		{name: "plan failed as asserted", run: "exit 1", wantErr: false},
		{name: "plan succeeded, so the assert fails", run: "true", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pipeline.yml")

			writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: %q
  assert:
    execution: [work]
    outcome: failed
`, test.run))

			err := cli.Run([]string{"run", path, "--job", "build"})
			if test.wantErr && err == nil {
				t.Fatal("job succeeded, want a failure: assert.outcome said failed but the plan passed")
			}

			if !test.wantErr && err != nil {
				t.Fatalf("job failed, want success: the plan failed exactly as asserted: %v", err)
			}
		})
	}
}

// TestJobAssertOutcomeSucceededOptsOutOfClearing verifies outcome: succeeded is
// not a no-op: a matching execution: normally clears a plan failure, and this
// is how a fixture says "no, that failure is real."
func TestJobAssertOutcomeSucceededOptsOutOfClearing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: exit 1
  assert:
    execution: [work]
    outcome: succeeded
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("job succeeded; a matching execution: must not clear the failure when outcome: succeeded is set")
	}

	if !strings.Contains(err.Error(), "assert.outcome: succeeded") {
		t.Errorf("error does not name the assertion that failed: %v", err)
	}
}

// TestJobAssertExecutionStillClearsWithoutOutcome pins the compatibility half:
// absent outcome:, a matching execution: clears the plan's failure exactly as
// it did before the field existed.
func TestJobAssertExecutionStillClearsWithoutOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: exit 1
  assert:
    execution: [work]
`)

	mustRun(t, path)
}

// TestJobAssertOutcomeAndExecutionCompose verifies a mismatch in EITHER
// directive fails the job, including when the other one holds.
func TestJobAssertOutcomeAndExecutionCompose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipeline.yml")

	// outcome: failed holds — the plan does fail — but execution: names a step
	// that never ran.
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: exit 1
  assert:
    execution: [work, never-ran]
    outcome: failed
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("job succeeded; a satisfied outcome: must not excuse a mismatched execution:")
	}

	if !strings.Contains(err.Error(), "assert.execution mismatch") {
		t.Errorf("want the execution mismatch reported, got: %v", err)
	}
}

// Every shipped example runs through `steps test` in TestDocsExamples
// (docs_test.go): the corpus is the fenced blocks of docs/*.md, extracted
// and executed — opt-out is per fence (`noexec`/`fragment`), each with the
// reason visible in the surrounding prose. examples/invalid/ (must-FAIL
// pipelines) stays separate; see TestExamplesInvalid.

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
    inputs: []
    run: "true"
  assert:
    execution: [work, wrong-name]
`)

	err := cli.Run([]string{"test", path})
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
    inputs: []
    run: "true"
`)

	err := cli.Run([]string{"test", path})
	if err == nil {
		t.Fatal("steps test succeeded, want a pipeline assert.execution mismatch")
	}

	// Sanity: the state dir the test run created lives beside the pipeline.
	_ = os.RemoveAll(filepath.Join(dir, ".steps"))
}

// TestPipelineAssertWithNoExecutionListAssertsNothing.
//
// `assert: execution:` omits by exclusion — a job listed nowhere in it must
// NOT have run — which raises a real question about the empty list: does it
// assert that nothing ran, or nothing at all? The code answers "nothing at
// all", and nothing pinned that answer, so the guard could be moved to the
// other reading and every suite stayed green while `steps test` began failing
// any pipeline carrying a bare assert block.
func TestPipelineAssertWithNoExecutionListAssertsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"

assert: {}
`)

	err := cli.Run([]string{"test", path})
	if err != nil {
		t.Fatalf("an assert block with no execution list failed the run: %v", err)
	}
}

// TestPipelineAssertExecutionStillChecksWhatItLists is the other half: a list
// that IS given is enforced, so the guard above cannot be satisfied by
// skipping the comparison entirely.
func TestPipelineAssertExecutionStillChecksWhatItLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"

assert:
  execution: [nosuchjob]
`)

	err := cli.Run([]string{"test", path})
	if err == nil {
		t.Fatal("a pipeline assert naming a job that never ran was reported as passing")
	}

	if !strings.Contains(err.Error(), "assert.execution mismatch") {
		t.Errorf("error does not report the mismatch: %v", err)
	}
}
