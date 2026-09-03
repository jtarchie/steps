package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// countLines returns the number of newline-terminated lines in path's
// contents, or 0 if the file doesn't exist yet.
func countLines(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped counter file this test wrote itself
	if os.IsNotExist(err) {
		return 0
	}

	if err != nil {
		t.Fatal(err)
	}

	return bytes.Count(data, []byte("\n"))
}

// assertLineCount fails the test if path doesn't have exactly want lines.
func assertLineCount(t *testing.T, path string, want int) {
	t.Helper()

	got := countLines(t, path)
	if got != want {
		t.Errorf("%s: got %d lines, want %d", filepath.Base(path), got, want)
	}
}

// mustRun calls run(args) and fails the test immediately if it errors.
func mustRun(t *testing.T, args ...string) {
	t.Helper()

	err := cli.Run(args)
	if err != nil {
		t.Fatalf("run(%v): %v", args, err)
	}
}

func TestRunJobSkipsUnchangedAndReexecutesOnChange(t *testing.T) {
	dir := t.TempDir()
	getCounter := filepath.Join(dir, "get-counter.txt")
	taskCounter := filepath.Join(dir, "task-counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	writePipeline := func(source string) {
		t.Helper()

		pipeline := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo fetched >> %s

resources:
- name: thing
  type: dummy
  source:
    key: %s

jobs:
- name: build
  plan:
  - get: thing
  - task: work
    inputs: []
    run: echo ran >> %s
`, getCounter, source, taskCounter)

		err := os.WriteFile(path, []byte(pipeline), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	writePipeline("v1")
	mustRun(t, path)
	assertLineCount(t, getCounter, 1)
	assertLineCount(t, taskCounter, 1)

	// Unchanged rerun: identical content hash, so both steps are skipped.
	mustRun(t, path)
	assertLineCount(t, getCounter, 1)
	assertLineCount(t, taskCounter, 1)

	// Changing the resource source changes the get node's hash, so both
	// steps (get, and the task chained after it) re-execute.
	writePipeline("v2")
	mustRun(t, path)
	assertLineCount(t, getCounter, 2)
	assertLineCount(t, taskCounter, 2)

	// --force bypasses the skip cache even though nothing changed.
	mustRun(t, "--force", path)
	assertLineCount(t, getCounter, 3)
	assertLineCount(t, taskCounter, 3)
}

func TestRunJobPutNeverSkipped(t *testing.T) {
	dir := t.TempDir()
	getCounter := filepath.Join(dir, "get-counter.txt")
	putCounter := filepath.Join(dir, "put-counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo fetched >> %s
    out: echo pushed >> %s

resources:
- name: thing
  type: dummy
  source:
    key: v1

jobs:
- name: build
  plan:
  - get: thing
  - put: thing
`, getCounter, putCounter)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
	assertLineCount(t, getCounter, 1)
	assertLineCount(t, putCounter, 1)

	mustRun(t, path)

	// get is an ancestor of the put in this chain, so it re-executes too:
	// whole-job skip granularity marks an entire chain non-skippable when
	// any node in it is a put, not just the put node itself.
	assertLineCount(t, getCounter, 2)
	assertLineCount(t, putCounter, 2)
}

// TestRunJobCheckCommandRunsOnceNotTwice: a get step's check: command must
// run at most once per RunJob invocation. Before the resource.Cache fix, it
// ran once during merkle.PlanChains (to hash the step) and again during
// runGetStep (to actually fetch it) — this counts every invocation of check:
// itself, independent of whether the step ends up skipped.
func TestRunJobCheckCommandRunsOnceNotTwice(t *testing.T) {
	dir := t.TempDir()
	checkCounter := filepath.Join(dir, "check-counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo ran >> %s; echo '[{"ref":"v1"}]'
    in: "true"

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: thing
`, checkCounter)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)
	assertLineCount(t, checkCounter, 1)

	// A changed --force run also re-executes check: exactly once, not twice
	// (force skips planning entirely, so the cache would just go unused —
	// this confirms that path still calls check: exactly once, not zero).
	mustRun(t, "--force", path)
	assertLineCount(t, checkCounter, 2)
}

// TestRunJobGuardDecidesWhenAnInputWasNeverProduced is the second half of what
// `when:` is for. A step's guard exists to answer "is there anything here to
// do", and the common shape of that question is about an artifact an earlier
// guarded step may or may not have written — a model that had nothing to say,
// a diff that came back empty. Staging the guard's own view used to demand
// every declared input first, so the guard on a step downstream of a skipped
// one failed before it could return false: the job errored on a missing
// directory instead of skipping the step that had nothing to publish.
//
// The step's own execution still requires its inputs. Only the guard tolerates
// an absent one, and only by reading it as absent.
func TestRunJobGuardDecidesWhenAnInputWasNeverProduced(t *testing.T) {
	dir := t.TempDir()
	published := filepath.Join(dir, "published.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: answer
    outputs: [answer]
    when: "false"
    run: printf 'something to say' > answer/reply.md
  - task: publish
    inputs: [answer]
    when: test -s answer/reply.md
    run: echo published >> %s
`, published)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)

	assertLineCount(t, published, 0)
}

// TestRunJobGuardSeesTheSameInputsTheStepDoes is the other half of the same
// contract, and the half a leniency rule can quietly break: an input that IS
// there has to reach the guard. A step's inputs: list is not the whole story —
// a task inherits its inputs from the tasks: entry it references, and
// input_mapping renames a declared input onto the plan artifact it draws from.
// A guard that reads the declared spelling against the artifact store finds
// nothing, answers false, and skips the step forever, with nothing in the log
// to say why.
func TestRunJobGuardSeesTheSameInputsTheStepDoes(t *testing.T) {
	dir := t.TempDir()
	published := filepath.Join(dir, "published.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
tasks:
- name: publish
  inputs: [answer]
  run: echo published >> %s

jobs:
- name: build
  plan:
  - task: scout
    outputs: [findings]
    run: printf 'something to say' > findings/reply.md
  - task: publish
    input_mapping: {answer: findings}
    when: test -s answer/reply.md
  assert:
    execution: [scout, publish]
    outcome: succeeded
`, published)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, path)

	assertLineCount(t, published, 1)
}
