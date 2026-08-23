package main

// The CLI-backed half of the assert.files: contract.
//
// A CLI agent runs its own tool loop in a subprocess, so there is no turn to
// interrupt: the equivalent moment is after the child exits, and the
// equivalent of "tell the model" is to REJOIN its session rather than start a
// new one. That distinction is the whole test — a restart would re-send the
// task to a model that has already done most of it, which is the failure mode
// attempts: was rebuilt to stop making (see docs/attempts-timeout.md).
//
// Not parallel: installing a fake binary means editing PATH for the process.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// cliAssertFilesPipeline is a CLI agent owing one file, and a task that
// copies it out so the test can see it survived capture.
func cliAssertFilesPipeline(t *testing.T, dir string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: responder
  source:
    model: "@claude/sonnet"
  tools: [write_file]

jobs:
- name: build
  plan:
  - agent: responder
    inputs: []
    outputs: [answer]
    messages:
      - Answer the question. Write your answer to answer/reply.md.
    assert:
      files: [answer/reply.md]
  - task: deliver
    inputs: [answer]
    run: cat answer/reply.md >> %[1]s
`, filepath.Join(dir, "delivered.log")))
}

// TestE2ECLIAgentNudgedIntoWritingItsFile drives the production failure
// against a CLI child: the first invocation answers in prose and writes
// nothing, and the step has to wake it up rather than accept the silence.
func TestE2ECLIAgentNudgedIntoWritingItsFile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "asked-once")

	// The child writes relative to its own working directory, which is the
	// step's — the same place assert.files: looks.
	cli := writeFakeClaude(t, fmt.Sprintf(`
if [ -f %[1]q ]; then
  mkdir -p answer
  echo 'The catalog is seeded from widgets.json.' > answer/reply.md
  echo '%[2]s'
else
  : > %[1]q
  echo '%[3]s'
fi
`, marker, cliResultEvent("Written.", 1), cliResultEvent("Here is the answer: it comes from widgets.json.", 1)))

	path := cliAssertFilesPipeline(t, dir)

	mustRun(t, path)

	if got := cli.invocations(t); got != 2 {
		t.Fatalf("the fake cli ran %d times, want 2 (the silent answer and the nudge)", got)
	}

	// Rejoined, not restarted — the session the first invocation opened is
	// the one the second continues.
	first, second := cli.argv(t, 1), cli.argv(t, 2)

	if !strings.Contains(first, "--session-id") {
		t.Errorf("first invocation did not open a session: %s", first)
	}

	if !strings.Contains(second, "--resume") {
		t.Errorf("the nudge started a new conversation instead of resuming: %s", second)
	}

	if session := cliSessionID(t, first); !strings.Contains(second, session) {
		t.Errorf("the nudge resumed a different session than the one opened (%s): %s", session, second)
	}

	// And it was told the one thing it could not see.
	if nudge := cli.prompt(t, 2); !strings.Contains(nudge, "answer/reply.md") {
		t.Errorf("the resumed prompt does not name the missing file: %q", nudge)
	}

	if got := readFileString(t, filepath.Join(dir, "delivered.log")); !strings.Contains(got, "widgets.json") {
		t.Errorf("downstream task did not receive the agent's reply; got %q", got)
	}
}

// TestE2ECLIAgentStillFailsWithoutItsFile is the other end: a child that
// never writes the file runs out of chances and the step fails, naming the
// path — the same verdict a CLI step reached before the nudge existed.
func TestE2ECLIAgentStillFailsWithoutItsFile(t *testing.T) {
	dir := t.TempDir()
	cli := writeFakeClaude(t, "echo '"+cliResultEvent("The answer is in this message.", 1)+"'")
	path := cliAssertFilesPipeline(t, dir)

	err := run([]string{path})
	if err == nil {
		t.Fatal("run() succeeded, but the agent never wrote its declared artifact")
	}

	if !strings.Contains(err.Error(), "answer/reply.md") {
		t.Errorf("failure does not name the missing file: %v", err)
	}

	// Bounded, and it did try more than once.
	got := cli.invocations(t)
	if got < 2 {
		t.Errorf("the fake cli ran %d times, want the first answer plus at least one nudge", got)
	}

	if got > 6 {
		t.Errorf("the fake cli ran %d times, want no more than 6 — the nudge is bounded", got)
	}

	assertNoFile(t, filepath.Join(dir, "delivered.log"))
}

// cliSessionID pulls the uuid out of a captured argv's --session-id pair.
// fakeCLI joins arguments with "|", so the value is the field after the flag.
func cliSessionID(t *testing.T, argv string) string {
	t.Helper()

	fields := strings.Split(argv, "|")
	for i, field := range fields {
		if field == "--session-id" && i+1 < len(fields) {
			return fields[i+1]
		}
	}

	t.Fatalf("no --session-id in %s", argv)

	return ""
}

// TestE2ECLIAgentNudgeSharesTheAttemptsBudget pins the ceiling when both
// budgets are in play. attempts: retries a child that DIED; the nudge wakes
// one that finished owing files. Nothing stops a round from doing both, and
// multiplying them is how "five chances" turns into eighteen real model
// invocations — each one paid for.
//
// They pool instead: a step spends at most attempts + maxFilesNudges child
// invocations, so a retry taken in one round is not handed back in the next.
// The fake fails every odd invocation and finishes every even one without
// writing, which is the compound the multiplication needs.
func TestE2ECLIAgentNudgeSharesTheAttemptsBudget(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "invocations")

	cli := writeFakeClaude(t, fmt.Sprintf(`
printf x >> %[1]q
if [ $(( $(wc -c < %[1]q) %% 2 )) -eq 1 ]; then
  exit 3
fi
echo '%[2]s'
`, counter, cliResultEvent("The answer is in this message.", 1)))

	path := writePipeline(t, dir, strings.Replace(
		readFileString(t, cliAssertFilesPipeline(t, dir)),
		"    assert:", "    attempts: 2\n    assert:", 1))

	err := run([]string{path})
	if err == nil {
		t.Fatal("run() succeeded, but the agent never wrote its declared artifact")
	}

	// 2 attempts + 5 nudge rounds, not 2 x 6.
	if got, want := cli.invocations(t), 2+5; got > want {
		t.Errorf("the fake cli ran %d times, want no more than %d — the two budgets pool rather than multiply", got, want)
	}
}
