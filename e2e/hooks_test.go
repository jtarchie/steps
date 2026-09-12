package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// hookPaths bundles the counter files a hook pipeline writes to, so each test
// can assert which hooks fired.
type hookPaths struct {
	task      string
	onSuccess string
	onFailure string
	onError   string
	ensure    string
	jobHook   string
}

func newHookPaths(dir string) hookPaths {
	return hookPaths{
		task:      filepath.Join(dir, "task.txt"),
		onSuccess: filepath.Join(dir, "on_success.txt"),
		onFailure: filepath.Join(dir, "on_failure.txt"),
		onError:   filepath.Join(dir, "on_error.txt"),
		ensure:    filepath.Join(dir, "ensure.txt"),
		jobHook:   filepath.Join(dir, "job_hook.txt"),
	}
}

func writePipelineFile(t *testing.T, path, pipeline string) {
	t.Helper()

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// wantRunError runs the pipeline at path and fails unless cli.Run returns an
// error (the job failed).
func wantRunError(t *testing.T, path string) {
	t.Helper()

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("cli.Run succeeded, want a failure")
	}
}

// TestStepHooksGreenTask verifies a green task fires on_success and ensure but
// not on_failure.
func TestStepHooksGreenTask(t *testing.T) {
	dir := t.TempDir()
	p := newHookPaths(dir)
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo ran >> %s
    on_success:
      task: ok
      run: echo ok >> %s
    on_failure:
      task: bad
      run: echo bad >> %s
    ensure:
      task: cleanup
      run: echo done >> %s
`, p.task, p.onSuccess, p.onFailure, p.ensure))

	mustRun(t, path)

	assertLineCount(t, p.task, 1)
	assertLineCount(t, p.onSuccess, 1)
	assertLineCount(t, p.ensure, 1)
	assertLineCount(t, p.onFailure, 0)
}

// TestStepHooksRedTask verifies a failing task fires on_failure and ensure
// (not on_success), and that the job still fails.
func TestStepHooksRedTask(t *testing.T) {
	dir := t.TempDir()
	p := newHookPaths(dir)
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo ran >> %s; exit 1
    on_success:
      task: ok
      run: echo ok >> %s
    on_failure:
      task: bad
      run: echo bad >> %s
    ensure:
      task: cleanup
      run: echo done >> %s
`, p.task, p.onSuccess, p.onFailure, p.ensure))

	wantRunError(t, path)

	assertLineCount(t, p.onFailure, 1)
	assertLineCount(t, p.ensure, 1)
	assertLineCount(t, p.onSuccess, 0)
}

// A hook agent's dir: fails at load like a plan agent's, not as a missing directory once the hook fires.
func TestStepHookAgentDirCheckedAtLoad(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, `
agents:
- name: reviewer
  source:
    model: openai/test-model
    endpoint: http://127.0.0.1:1
    api_key_env: STEPS_TEST_AGENT_API_KEY
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: "true"
    on_failure:
      agent: reviewer
      dir: nowhere
      messages:
        - "why did it fail?"
`)

	err := cli.Run([]string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), `on_failure hook agent "reviewer"`) {
		t.Fatalf("validate: %v, want the hook agent's dir: refused", err)
	}
}

// TestStepHooksOnSuccessFailureFailsGreenStep verifies a failing on_success hook turns an otherwise-green step into a failure.
func TestStepHooksOnSuccessFailureFailsGreenStep(t *testing.T) {
	dir := t.TempDir()
	p := newHookPaths(dir)
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo ran >> %s
    on_success:
      task: ok
      run: echo ok >> %s; exit 1
    ensure:
      task: cleanup
      run: echo done >> %s
`, p.task, p.onSuccess, p.ensure))

	wantRunError(t, path)

	// ensure still runs even though on_success failed.
	assertLineCount(t, p.ensure, 1)
}

// TestStepHooksNotFiredWhenSkipped verifies a cached (skipped) task fires no
// step hooks on a second run, while the job-level on_success still fires.
func TestStepHooksNotFiredWhenSkipped(t *testing.T) {
	dir := t.TempDir()
	p := newHookPaths(dir)
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo ran >> %s
    on_success:
      task: ok
      run: echo ok >> %s
  on_success:
    task: announce
    run: echo announced >> %s
`, p.task, p.onSuccess, p.jobHook))

	mustRun(t, path)
	assertLineCount(t, p.task, 1)
	assertLineCount(t, p.onSuccess, 1)
	assertLineCount(t, p.jobHook, 1)

	// Second run: the task is cached/skipped, so its step on_success hook does
	// not fire again — but the job on_success (never hashed) fires every run.
	mustRun(t, path)
	assertLineCount(t, p.task, 1)
	assertLineCount(t, p.onSuccess, 1)
	assertLineCount(t, p.jobHook, 2)
}

// TestJobHooksOnFailure verifies a job-level on_failure fires when a plan step
// fails, and the job still reports failure.
func TestJobHooksOnFailure(t *testing.T) {
	dir := t.TempDir()
	p := newHookPaths(dir)
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: exit 1
  on_failure:
    task: alert
    run: echo alerted >> %s
  on_success:
    task: announce
    run: echo announced >> %s
`, p.onFailure, p.onSuccess))

	wantRunError(t, path)

	assertLineCount(t, p.onFailure, 1)
	assertLineCount(t, p.onSuccess, 0)
}

// TestStepHookEditRerunsParent verifies that editing a hook busts the parent
// step's merkle cache, re-running the parent (and its hooks) on the next run.
func TestStepHookEditRerunsParent(t *testing.T) {
	dir := t.TempDir()
	p := newHookPaths(dir)
	path := pipelinePath(t, dir)

	write := func(hookMsg string) {
		t.Helper()

		writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo ran >> %s
    on_success:
      task: ok
      run: echo %s >> %s
`, p.task, hookMsg, p.onSuccess))
	}

	write("first")
	mustRun(t, path)
	assertLineCount(t, p.task, 1)

	// Editing the hook changes the parent task's content hash, so it re-runs.
	write("second")
	mustRun(t, path)
	assertLineCount(t, p.task, 2)
	assertLineCount(t, p.onSuccess, 2)
}
