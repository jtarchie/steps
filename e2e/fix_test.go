package e2e

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// captures is each test's own stdout while it is inside captureStdout, by test name. A map rather than a swapped os.Stdout so that a test capturing its output can run beside another: the process's stream is one, and stepsRun hands each invocation the buffer of the test that made it.
var captures sync.Map //nolint:gochecknoglobals // the registry stepsRun reads; keyed per test, so it is shared by nobody

// captureStdout returns what the invocations fn makes through stepsRun print to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	buffer := &lockedBuffer{}

	previous, nested := captures.Swap(t.Name(), buffer)
	fn()

	if nested {
		captures.Store(t.Name(), previous)
	} else {
		captures.Delete(t.Name())
	}

	return buffer.String()
}

// stepsRun is one steps invocation, in process: cli.Run writing to the calling test's capture when it is inside captureStdout (its own, or the test it is a subtest of), and to the process's streams otherwise.
func stepsRun(t *testing.T, args []string) error {
	t.Helper()

	return cli.RunWith(streamsFor(t), args) //nolint:wrapcheck // the test asserts on the command's own error
}

// streamsFor is where an invocation t makes writes: t's capture while one is open, the process's streams otherwise.
func streamsFor(t *testing.T) cli.Streams {
	t.Helper()

	streams := cli.Streams{Out: os.Stdout, Err: os.Stderr}

	for name := t.Name(); ; name = name[:strings.LastIndex(name, "/")] {
		if buffer, ok := captures.Load(name); ok {
			streams.Out, _ = buffer.(io.Writer)

			break
		}

		if !strings.Contains(name, "/") {
			break
		}
	}

	return streams
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

	return writeFixAssertPipeline(t, dir, endpoint, run, "", "")
}

// writeFixAssertPipeline is writeFixPipeline plus an optional step assert:
// and job assert: — the combination that used to bind nothing. Both blocks
// are given unindented ("code: 0\nstdout: run 2") and indented here, so a
// call site reads as the YAML it becomes and cannot silently produce an
// empty `assert:` (which loads as null, leaving the step with no assert at
// all and the test quietly exercising a path it does not name).
func writeFixAssertPipeline(t *testing.T, dir, endpoint, run, stepAssert, jobAssert string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
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
%s%s`, endpoint, run, indentBlock(stepAssert, "    "), indentBlock(jobAssert, "  ")))
}

// indentBlock renders one optional YAML block under its own `assert:` key at
// the given indent, or nothing at all when the block is empty.
func indentBlock(block, indent string) string {
	if block == "" {
		return ""
	}

	var out strings.Builder

	out.WriteString(indent + "assert:\n")

	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		out.WriteString(indent + "  " + line + "\n")
	}

	return out.String()
}

// failThenPass returns a command that exits 1 the first time it runs and 0
// the second, printing which run it is on so an assert can tell them apart.
// The counter path is quoted: t.TempDir() derives from TMPDIR, and a space in
// it would otherwise turn the fixture's semantic assertion into a shell parse
// error wearing an assert mismatch's clothes.
func failThenPass(counter string) string {
	return fmt.Sprintf(
		`c='%s'; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; echo "run $n"; test $n -ge 2`,
		counter)
}

// TestRunJobTaskFixRecovers: the task fails on its first run and passes on
// the re-run (a counter file makes the command fail-then-pass), so the fix
// agent is invoked exactly once and the job succeeds. Not parallel: uses
// t.Setenv.
func TestRunJobTaskFixRecovers(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixPipeline(t, dir, fake.URL, failThenPass(counter))

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1", got)
	}

	if got := strings.TrimSpace(readFileString(t, counter)); got != "2" {
		t.Errorf("counter = %q, want 2 (command should have run twice: initial + verdict re-run)", got)
	}
}

// TestRunJobTaskFixPrintsResponse: the fix agent's final response ("done",
// per fixAgentServer) must reach the terminal, not just the retry/error log —
// previously RunFix discarded runAgentConversation's result entirely.
func TestRunJobTaskFixPrintsResponse(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixPipeline(t, dir, fake.URL, failThenPass(counter))

	// Not mustRun: captureStdout's os.Stdout restore only runs if fn returns
	// normally, and t.Fatalf inside fn would Goexit past it, leaving stdout
	// swapped for every later test in this package (see the same caution in
	// internal/trigger/trigger_test.go's captureStdout usage).
	var runErr error

	output := captureStdout(t, func() { runErr = stepsRun(t, []string{path}) })

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
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	path := writeFixPipeline(t, dir, fake.URL, "true")

	mustRun(t, path)

	if got := fake.requestCount(); got != 0 {
		t.Errorf("fix agent calls = %d, want 0 (a passing task must not invoke the agent)", got)
	}
}

// TestRunJobTaskFixStillFailing: a task that always fails invokes the agent
// once, but the verdict re-run still fails, so the job errors.
func TestRunJobTaskFixStillFailing(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	path := writeFixPipeline(t, dir, fake.URL, "false")

	err := stepsRun(t, []string{path})
	if err == nil {
		t.Fatal("expected the job to fail when the task still fails after the fix agent")
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1", got)
	}
}

// TestRunJobTaskFixRunsBeforeAssertJudges: a task carrying both is repaired
// first and judged second. assert: is an oracle over the step's outcome and
// fix: is part of producing one, so an assert that suppressed the repair was
// a declared fixer that bound nothing — with no error to say so.
//
// It also pins WHICH run the oracle sees: the final one, the same rule
// attempts: already follows.
func TestRunJobTaskFixRunsBeforeAssertJudges(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixAssertPipeline(t, dir, fake.URL, failThenPass(counter),
		"code: 0\nstdout: run 2",
		"execution: [fixer, check]\noutcome: succeeded")

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1 — the assert suppressed the repair", got)
	}

	if got := strings.TrimSpace(readFileString(t, counter)); got != "2" {
		t.Errorf("counter = %q, want 2 (initial run + the post-repair re-run)", got)
	}
}

// TestRunJobTaskFixAssertStillJudges: the repair is not a free pass. A
// command the fixer could not make satisfy the assert still fails the step,
// and the reason is the assert's own mismatch — which is what the author
// declared the step's success to mean.
func TestRunJobTaskFixAssertStillJudges(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixAssertPipeline(t, dir, fake.URL, failThenPass(counter),
		"stdout: run 3", "")

	err := stepsRun(t, []string{path})
	if err == nil {
		t.Fatal("expected the job to fail: the repaired command still does not satisfy the assert")
	}

	if !strings.Contains(err.Error(), "assert.stdout") {
		t.Errorf("failure reason = %v, want the assert's own mismatch", err)
	}

	// A step its fixer already tried and failed to rescue must not read like a
	// step that never had one — that is the first question a red build of a
	// per-invocation-billed feature raises.
	if !strings.Contains(err.Error(), `still failing after fix agent "fixer"`) {
		t.Errorf("failure reason = %v, want it to name the fix agent that could not rescue the step", err)
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1", got)
	}
}

// TestRunJobTaskFixGreenPathSkipsAgentWithAssert: the $0 happy path survives
// the composition — a command that passes first try never constructs the
// agent, assert or no assert. A regression guard rather than a change
// detector: it holds under either dispatch order, which the two tests below
// it do not.
func TestRunJobTaskFixGreenPathSkipsAgentWithAssert(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	path := writeFixAssertPipeline(t, dir, fake.URL, "echo all good",
		"code: 0\nstdout: all good",
		"execution: [check]\noutcome: succeeded")

	mustRun(t, path)

	if got := fake.requestCount(); got != 0 {
		t.Errorf("fix agent calls = %d, want 0 (a passing task must not invoke the agent)", got)
	}
}

// TestRunJobTaskFixRepairsAnAssertMissAtExitZero: a command that exits 0 and
// still fails the step. `run:` succeeded, the step did not — assert: is what
// the author said success means, so it is what a repair has to be triggered
// by. Gated on the exit code instead, this is a declared fixer that binds
// nothing: the one failure mode the author wrote down is the one the fixer
// never hears about.
func TestRunJobTaskFixRepairsAnAssertMissAtExitZero(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	// Exits 0 every time; only the SECOND run prints what the assert wants.
	run := fmt.Sprintf(`c='%s'; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; echo "run $n"`, counter)
	path := writeFixAssertPipeline(t, dir, fake.URL, run,
		"stdout: run 2",
		"execution: [fixer, check]\noutcome: succeeded")

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("fix agent calls = %d, want 1 — an exit-0 run that misses its assert is a failed step", got)
	}

	if got := strings.TrimSpace(readFileString(t, counter)); got != "2" {
		t.Errorf("counter = %q, want 2 (initial run + the post-repair re-run)", got)
	}
}

// TestRunJobTaskFixSkipsAnAssertedNonZeroExit: the converse, and the one that
// costs money to get wrong. `assert: {code: N}` is the documented way to say
// "this command is SUPPOSED to exit N"; a run the assert already accepts is a
// green step, so no repair is owed and no model is called. Triggering on the
// raw exit code also inverts the outcome — a fixer that succeeded at driving
// the command to exit 0 would fail `code: 1`, turning the repair itself into
// the reason a passing pipeline went red.
func TestRunJobTaskFixSkipsAnAssertedNonZeroExit(t *testing.T) {
	t.Parallel()

	fake := fixAgentServer(t)

	dir := t.TempDir()
	counter := filepath.Join(dir, "counter.txt")
	path := writeFixAssertPipeline(t, dir, fake.URL, failThenPass(counter),
		"code: 1\nstdout: run 1",
		"execution: [check]\noutcome: succeeded")

	mustRun(t, path)

	if got := fake.requestCount(); got != 0 {
		t.Errorf("fix agent calls = %d, want 0 — the assert already called this run a success", got)
	}

	if got := strings.TrimSpace(readFileString(t, counter)); got != "1" {
		t.Errorf("counter = %q, want 1 (no repair means no verdict re-run)", got)
	}
}
