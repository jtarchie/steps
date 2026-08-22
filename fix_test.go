package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe, returning
// everything fn wrote via fmt.Printf and friends. Not safe alongside other
// tests running in parallel that also touch os.Stdout — callers must not use
// t.Parallel(). Duplicated per-package (see internal/agent/step_test.go,
// internal/trigger/trigger_test.go) rather than exported cross-package.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()

	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

// fixAgentServer returns a fake provider that answers every request with a
// plain "done" (no tool calls). A fix agent that just says "done" does no
// repair itself — so a task that recovers proves the control flow
// (fail -> invoke -> re-run), independent of the model's actual fixing
// ability. Repeating rather than scripted because these tests assert on how
// many times the agent was reached, not on what it was told.
func fixAgentServer(t *testing.T) *fakeLLM {
	t.Helper()

	return newRepeatingFakeLLM(t, says("done"))
}

func writeFixPipeline(t *testing.T, dir, endpoint, run string) string {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")
	pipeline := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

jobs:
- name: build
  plan:
  - task: check
    inputs: []
    run: %s
    fix: fixer
`, endpoint, run)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// TestRunJobTaskFixRecovers: the task fails on its first run and passes on
// the re-run (a counter file makes the command fail-then-pass), so the fix
// agent is invoked exactly once and the job succeeds. Not parallel: uses
// t.Setenv.
func TestRunJobTaskFixRecovers(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	// Fail (exit 1) on the first invocation, pass (exit 0) on the second.
	run := fmt.Sprintf(`c=%s; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; test $n -ge 2`, counter)
	path := writeFixPipeline(t, dir, fake.URL, run)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1", got)
	}

	if got := strings.TrimSpace(readFile(t, counter)); got != "2" {
		t.Errorf("counter = %q, want 2 (command should have run twice: initial + verdict re-run)", got)
	}
}

// TestRunJobTaskFixPrintsResponse: the fix agent's final response ("done",
// per fixAgentServer) must reach the terminal, not just the retry/error log —
// previously RunFix discarded runAgentConversation's result entirely. Not
// t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunJobTaskFixPrintsResponse(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	runCmd := fmt.Sprintf(`c=%s; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; test $n -ge 2`, counter)
	path := writeFixPipeline(t, dir, fake.URL, runCmd)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	// Not mustRun: captureStdout's os.Stdout restore only runs if fn returns
	// normally, and t.Fatalf inside fn would Goexit past it, leaving stdout
	// swapped for every later test in this package (see the same caution in
	// internal/trigger/trigger_test.go's captureStdout usage).
	var runErr error

	output := captureStdout(t, func() { runErr = run([]string{path}) })

	if runErr != nil {
		t.Fatalf("run(%v): %v", []string{path}, runErr)
	}

	if !strings.Contains(output, "done") {
		t.Errorf("stdout = %q, want it to contain the fix agent's response %q", output, "done")
	}
}

// TestRunJobTaskFixGreenPathSkipsAgent: a task that passes on its first run
// never constructs the fix agent — the $0 happy path.
func TestRunJobTaskFixGreenPathSkipsAgent(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	path := writeFixPipeline(t, dir, fake.URL, "true")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 0 {
		t.Errorf("fix agent calls = %d, want 0 (a passing task must not invoke the agent)", got)
	}
}

// TestRunJobTaskFixStillFailing: a task that always fails invokes the agent
// once, but the verdict re-run still fails, so the job errors.
func TestRunJobTaskFixStillFailing(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	path := writeFixPipeline(t, dir, fake.URL, "false")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("expected the job to fail when the task still fails after the fix agent")
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// writeFixAssertPipeline is writeFixPipeline plus a step assert: and a
// job-level execution assert — the combination that used to bind nothing.
func writeFixAssertPipeline(t *testing.T, dir, endpoint, run, stepAssert, jobAssert string) string {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")
	pipeline := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

jobs:
- name: build
  plan:
  - task: check
    inputs: []
    run: %s
    fix: fixer
    assert:
%s%s
`, endpoint, run, stepAssert, jobAssert)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// failThenPass returns a command that exits 1 the first time it runs and 0
// the second, printing which run it is on so an assert can tell them apart.
func failThenPass(counter string) string {
	return fmt.Sprintf(
		`c=%s; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; echo "run $n"; test $n -ge 2`,
		counter)
}

// TestRunJobTaskFixRunsBeforeAssertJudges: a task carrying both is repaired
// first and judged second. assert: is an oracle over the step's outcome and
// fix: is part of producing one, so an assert that suppressed the repair was
// a declared fixer that bound nothing — with no error to say so.
//
// It also pins WHICH run the oracle sees: the final one, the same rule
// attempts: already follows.
func TestRunJobTaskFixRunsBeforeAssertJudges(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixAssertPipeline(t, dir, fake.URL, failThenPass(counter),
		"      code: 0\n      stdout: run 2",
		"\n  assert:\n    execution: [fixer, check]\n    outcome: succeeded")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1 — the assert suppressed the repair", got)
	}

	if got := strings.TrimSpace(readFile(t, counter)); got != "2" {
		t.Errorf("counter = %q, want 2 (initial run + the post-repair re-run)", got)
	}
}

// TestRunJobTaskFixAssertStillJudges: the repair is not a free pass. A
// command the fixer could not make satisfy the assert still fails the step,
// and the reason is the assert's own mismatch — which is what the author
// declared the step's success to mean.
func TestRunJobTaskFixAssertStillJudges(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixAssertPipeline(t, dir, fake.URL, failThenPass(counter),
		"      stdout: run 3", "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("expected the job to fail: the repaired command still does not satisfy the assert")
	}

	if !strings.Contains(err.Error(), "assert.stdout") {
		t.Errorf("failure reason = %v, want the assert's own mismatch", err)
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1", got)
	}
}

// TestRunJobTaskFixGreenPathSkipsAgentWithAssert: the $0 happy path survives
// the composition — a command that passes first try never constructs the
// agent, assert or no assert.
func TestRunJobTaskFixGreenPathSkipsAgentWithAssert(t *testing.T) {
	fake := fixAgentServer(t)

	dir := t.TempDir()
	path := writeFixAssertPipeline(t, dir, fake.URL, "echo all good",
		"      code: 0\n      stdout: all good",
		"\n  assert:\n    execution: [check]\n    outcome: succeeded")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.requestCount(); got != 0 {
		t.Errorf("fix agent calls = %d, want 0 (a passing task must not invoke the agent)", got)
	}
}
