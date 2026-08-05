package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

			err := run([]string{"run", path, "--job", "build"})
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

	err := run([]string{"run", path, "--job", "build"})
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

	err := run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("job succeeded; a satisfied outcome: must not excuse a mismatched execution:")
	}

	if !strings.Contains(err.Error(), "assert.execution mismatch") {
		t.Errorf("want the execution mismatch reported, got: %v", err)
	}
}

// exampleSkipMarker opts an example out of TestExamplesRun. It is deliberately
// OPT-OUT: an example that says nothing must run offline and pass. An opt-in
// marker would have let examples/try.yml ship calling `make build` in a repo
// with no Makefile — unrunnable, and silently uncovered, because an unmarked
// file would simply not have been run.
//
// The reason text after the marker is for a human reading the file; only the
// marker itself is matched.
const exampleSkipMarker = "# steps-test: skip"

// TestExamplesRun runs every shipped example through `steps test`, except
// those explicitly marked as needing something this suite has not got.
//
// README.md and CLAUDE.md both promise the examples are runnable and
// self-contained, and several are self-verifying regression fixtures. Nothing
// enforced either half: TestValidateExamples proved they PARSE, and two
// hardcoded tests ran two of them. A non-runnable example passes `go fmt`,
// `go mod tidy`, `golangci-lint`, `go test` and `go build` without complaint,
// because none of those execute it — which is exactly how one shipped.
//
// Adding an example therefore opts it into this test automatically, and
// exempting one costs a visible line in the file saying why.
//
// The glob is deliberately non-recursive: examples/invalid/ holds pipelines
// that must FAIL to load (see TestExamplesInvalid), and including them here
// would pin this test red forever.
func TestExamplesRun(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("examples", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no examples found")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			body, readErr := os.ReadFile(path) //nolint:gosec // repo-owned example file
			if readErr != nil {
				t.Fatal(readErr)
			}

			if strings.Contains(string(body), exampleSkipMarker) {
				t.Skipf("%s opts out (%s)", path, exampleSkipMarker)
			}

			// Each example runs in its own directory so the state.db a run
			// creates lands in a temp dir rather than examples/.
			dir := t.TempDir()
			dst := filepath.Join(dir, filepath.Base(path))

			writeErr := os.WriteFile(dst, body, 0o600) //nolint:gosec // dst is under t.TempDir(), name from a repo-owned glob
			if writeErr != nil {
				t.Fatal(writeErr)
			}

			copyExampleDeps(t, filepath.Dir(path), dir)

			runErr := run([]string{"test", dst})
			if runErr != nil {
				t.Errorf("steps test %s: %v", path, runErr)
			}
		})
	}
}

// copyExampleDeps mirrors the sibling files an example loads from disk —
// examples/tasks/ holds the run_file: targets flow.yml references — into the
// temp directory the example is executed from, since those paths resolve
// against the pipeline file's own directory.
func copyExampleDeps(t *testing.T, srcDir, dstDir string) {
	t.Helper()

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		// examples/invalid/ holds must-fail pipelines, not deps of a runnable
		// example — no example loads anything from it.
		if !entry.IsDir() || entry.Name() == "invalid" {
			continue
		}

		sub := filepath.Join(dstDir, entry.Name())

		mkErr := os.MkdirAll(sub, 0o750)
		if mkErr != nil {
			t.Fatal(mkErr)
		}

		files, readErr := os.ReadDir(filepath.Join(srcDir, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}

		for _, file := range files {
			body, fileErr := os.ReadFile(filepath.Join(srcDir, entry.Name(), file.Name())) //nolint:gosec // repo-owned example file
			if fileErr != nil {
				t.Fatal(fileErr)
			}

			//nolint:gosec // sub is under t.TempDir(), name from a repo-owned dir listing
			wErr := os.WriteFile(filepath.Join(sub, file.Name()), body, 0o600)
			if wErr != nil {
				t.Fatal(wErr)
			}
		}
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
    inputs: []
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
    inputs: []
    run: "true"
`)

	err := run([]string{"test", path})
	if err == nil {
		t.Fatal("steps test succeeded, want a pipeline assert.execution mismatch")
	}

	// Sanity: the state dir the test run created lives beside the pipeline.
	_ = os.RemoveAll(filepath.Join(dir, ".steps"))
}
