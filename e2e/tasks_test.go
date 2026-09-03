package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestRunJobTaskReferenceRunsNamedTask: a task step with no run: resolves
// against a top-level tasks: entry of the same name and runs its command.
func TestRunJobTaskReferenceRunsNamedTask(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
tasks:
- name: unit
  run: echo ran >> %s

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
`, counter)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
	assertLineCount(t, counter, 1)

	// Unchanged rerun: same resolved content, so the task is skipped.
	mustRun(t, path)
	assertLineCount(t, counter, 1)
}

// TestRunJobTaskInlineIgnoresSameNamedTopLevelTask: a step that supplies its
// own run: is always inline, even when a tasks: entry of the same name
// exists — the top-level entry must never be consulted.
func TestRunJobTaskInlineIgnoresSameNamedTopLevelTask(t *testing.T) {
	dir := t.TempDir()
	topLevelCounter := filepath.Join(dir, "top-level-counter.txt")
	inlineCounter := filepath.Join(dir, "inline-counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
tasks:
- name: unit
  run: echo top-level >> %s

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
    run: echo inline >> %s
`, topLevelCounter, inlineCounter)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
	assertLineCount(t, topLevelCounter, 0)
	assertLineCount(t, inlineCounter, 1)
}

// TestRunJobTaskRunFileEditBustsCache is the load-time run_file: feature's
// payoff test: merkle.TaskNodeContent hashes rt.Run, and after
// resolveFileIncludes runs (at LoadConfig, well before PlanChains), rt.Run IS
// the included file's contents — so editing that file must be indistinguishable
// from editing an inline run: for cache purposes. steps test/--force never
// exercise PlanChains at all (skipCache=true), so this has to go through the
// real cli.Run path, not a fixture.
func TestRunJobTaskRunFileEditBustsCache(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	scriptPath := filepath.Join(dir, "unit.sh")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := `
tasks:
- name: unit
  run_file: unit.sh

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writeScript := func(body string) {
		t.Helper()

		err := os.WriteFile(scriptPath, []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	writeScript(fmt.Sprintf("echo ran >> %s\n", counter))
	mustRun(t, path)
	assertLineCount(t, counter, 1)

	// Unchanged rerun: same resolved content, so the task is skipped.
	mustRun(t, path)
	assertLineCount(t, counter, 1)

	// Editing the included script changes what rt.Run resolves to, exactly
	// like editing an inline run: would — the cache must not treat this as
	// the same step.
	writeScript(fmt.Sprintf("echo ran-again >> %s\n", counter))
	mustRun(t, path)
	assertLineCount(t, counter, 2)
}

// TestRunJobTaskRunFileRenameDoesNotBustCache pins "contents, not paths" as
// intentional (see internal/config's readIncludeFile doc comment): renaming
// the included file to an identical-bytes copy at a new path, and updating
// run_file: to match, must NOT invalidate the cache, since nothing about
// rt.Run's resolved text changed.
func TestRunJobTaskRunFileRenameDoesNotBustCache(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	writePipeline := func(scriptName string) {
		t.Helper()

		pipeline := fmt.Sprintf(`
tasks:
- name: unit
  run_file: %s

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
`, scriptName)

		err := os.WriteFile(path, []byte(pipeline), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	body := fmt.Sprintf("echo ran >> %s\n", counter)

	err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writePipeline("a.sh")
	mustRun(t, path)
	assertLineCount(t, counter, 1)

	err = os.WriteFile(filepath.Join(dir, "b.sh"), []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writePipeline("b.sh")
	mustRun(t, path)
	assertLineCount(t, counter, 1)
}

// TestRunJobTaskReferenceUndefinedErrors: a task step naming an undefined
// tasks: entry (and no run: of its own) fails clearly at plan time.
func TestRunJobTaskReferenceUndefinedErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := `
jobs:
- name: build
  plan:
  - task: missing
    inputs: []
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = cli.Run([]string{path})
	if err == nil {
		t.Fatal("expected an error for a task step referencing an undefined tasks: entry")
	}

	if got := err.Error(); !strings.Contains(got, `no task named "missing"`) {
		t.Errorf("error = %q, want it to mention no task named %q", got, "missing")
	}
}

// writeTaskFixPipeline is like writeFixPipeline (fix_test.go) but the job
// step references a top-level tasks: entry (task: unit, no run:) instead of
// declaring run:/fix: inline. taskFix is the top-level task's own fix: value
// (verbatim YAML, e.g. "fixer" or a mapping); stepFixLine, if non-empty, is
// appended as the step's own fix: override.
func writeTaskFixPipeline(t *testing.T, dir, endpointA, endpointB, run, taskFix, stepFixLine string) string {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")
	pipeline := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixerA
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]
- name: fixerB
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

tasks:
- name: unit
  run: %s
  fix: %s

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
%s`, endpointA, endpointB, run, taskFix, stepFixLine)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// TestRunJobTaskReferenceUsesTaskFix: a referenced task's own fix: is used
// when the step sets none of its own.
func TestRunJobTaskReferenceUsesTaskFix(t *testing.T) {
	fakeA := fixAgentServer(t)
	fakeB := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	run := failThenPass(counter)
	path := writeTaskFixPipeline(t, dir, fakeA.URL, fakeB.URL, run, "fixerA", "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fakeA.requestCount(); got != 1 {
		t.Errorf("fixerA calls = %d, want 1 (the task's own fix:)", got)
	}

	if got := fakeB.requestCount(); got != 0 {
		t.Errorf("fixerB calls = %d, want 0 (not referenced anywhere)", got)
	}
}

// TestRunJobTaskReferenceStepFixOverridesTaskFix: a step's own fix: overrides
// the referenced task's fix: for that step only.
func TestRunJobTaskReferenceStepFixOverridesTaskFix(t *testing.T) {
	fakeA := fixAgentServer(t)
	fakeB := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	run := failThenPass(counter)
	path := writeTaskFixPipeline(t, dir, fakeA.URL, fakeB.URL, run, "fixerA", "    fix: fixerB\n")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fakeB.requestCount(); got != 1 {
		t.Errorf("fixerB calls = %d, want 1 (the step's override)", got)
	}

	if got := fakeA.requestCount(); got != 0 {
		t.Errorf("fixerA calls = %d, want 0 (overridden by the step's fix:)", got)
	}
}
