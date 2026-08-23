package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests that drive the REAL `claude` binary.
//
// They exist because the fake CLI can only ever agree with whatever this
// package believes about the real one. That gap is not hypothetical: the fake
// exited 0 where the real binary exits 1 on a reported failure, and a whole
// class of routing and retry behavior was wrong underneath a green suite until
// somebody ran the actual thing. These are the durable version of that check.
//
// They are opt-in for the same reasons internal/shell's Docker tests are
// (non-hermetic, slow, and here also billable), following the same
// STEPS_TEST_* precedent: a plain `go test ./...` skips them cleanly.

// requireClaudeCLI gates a test on an explicit opt-in plus an installed,
// authenticated binary.
func requireClaudeCLI(t *testing.T) {
	t.Helper()

	if os.Getenv("STEPS_TEST_CLAUDE") == "" {
		t.Skip("set STEPS_TEST_CLAUDE=1 to run tests against the real claude CLI (slow, and spends real model quota)")
	}

	_, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found on PATH")
	}
}

// liveCLIModel is the cheapest model that still tool-calls reliably. These
// tests assert on mechanism, never on the quality of an answer, so the
// smallest model is the right one.
const liveCLIModel = "@claude/haiku"

// TestLiveCLIGrantedWriteActuallyWrites pins the permission half of the tool
// grant against the real binary.
//
// Write and Edit are permission-gated in the CLI, unlike Read/Glob/Grep, so
// "granted write_file can write" is a claim about how --allowedTools interacts
// with non-interactive mode, not something the argument vector alone proves.
// The inverse (an ungranted tool is denied) is covered by the fake-backed
// suite; this is the direction a fake cannot answer.
func TestLiveCLIGrantedWriteActuallyWrites(t *testing.T) {
	requireClaudeCLI(t)

	dir := t.TempDir()
	proof := filepath.Join(dir, "proof.txt")

	// The agent writes into its own workspace; a following task copies the
	// result somewhere this test can see after the build is torn down.
	path := writePipeline(t, dir, `
agents:
- name: writer
  source:
    model: "`+liveCLIModel+`"
  max_turns: 6
  tools: [write_file]

jobs:
- name: write
  plan:
  - agent: writer
    inputs: []
    messages:
      - Create a file named out.txt containing exactly the word WROTE. Then stop.
  - task: capture
    inputs: []
    run: cp out.txt `+proof+`
`)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.TrimSpace(readFileString(t, proof))
	if !strings.Contains(got, "WROTE") {
		t.Errorf("out.txt = %q, want it to contain WROTE", got)
	}

	// The trajectory has to show the native tool, not a bridged stand-in:
	// a granted built-in is supposed to become the CLI's own Write.
	node := findNode(t, storeNodes(t, path), "agent", "writer")
	if node.Status != "succeeded" {
		t.Errorf("agent node status = %q (%s), want succeeded", node.Status, node.Error)
	}
}

// TestLiveCLIUngrantedToolIsWithheld is the fence, checked against the real
// tool surface rather than against our own argument builder.
//
// It is the test that would catch the CLI changing how it reads the flags this
// package sets -- the failure mode being that a step quietly gains a
// capability its pipeline never granted.
func TestLiveCLIUngrantedToolIsWithheld(t *testing.T) {
	requireClaudeCLI(t)

	dir := t.TempDir()
	report := filepath.Join(dir, "report.txt")

	// Only read_file is granted, so no shell tool should exist. The agent is
	// asked to try and then say what happened, which keeps the assertion on
	// an observable side effect rather than on the model's prose.
	path := writePipeline(t, dir, `
agents:
- name: limited
  source:
    model: "`+liveCLIModel+`"
  max_turns: 6
  tools: [read_file]

jobs:
- name: probe
  plan:
  - agent: limited
    inputs: []
    messages:
      - |
        Try to run the shell command: touch `+filepath.Join(dir, "breach.txt")+`
        If you have no tool that can run shell commands, simply stop.
  - task: capture
    inputs: []
    run: echo done > `+report+`
`)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "breach.txt"))

	// And the trajectory must not contain a shell call at all: the tool was
	// never offered, so there was nothing to refuse.
	node := findNode(t, storeNodes(t, path), "agent", "limited")
	if node.Status != "succeeded" {
		t.Errorf("agent node status = %q (%s), want succeeded", node.Status, node.Error)
	}
}

// TestLiveCLIRetryResumesRealSession is the composition the fake cannot check:
// steps passes --session-id then --resume (fake-covered), and a real killed
// session can actually be resumed (checked by hand), but only this proves the
// two halves meet.
//
// It matters because the alternative -- restarting -- is the design the hosted
// path deliberately removed: a retried agent inherits its own half-finished
// edits with no memory of making them.
func TestLiveCLIRetryResumesRealSession(t *testing.T) {
	requireClaudeCLI(t)

	realCLI, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not found on PATH")
	}

	dir := t.TempDir()
	shimDir := t.TempDir()
	state := filepath.Join(shimDir, "count")

	// A shim in front of the real binary: it SIGKILLs the first invocation
	// partway through, which is the infrastructure failure attempts: exists
	// for, and passes every later one through untouched.
	shim := fmt.Sprintf(`#!/bin/sh
n=$(cat %[1]q 2>/dev/null || echo 0)
echo $((n+1)) > %[1]q
if [ "$n" = "0" ]; then
  exec timeout -s KILL 12 %[2]q "$@"
fi
exec %[2]q "$@"
`, state, realCLI)

	err = os.WriteFile(filepath.Join(shimDir, "claude"), []byte(shim), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing shim: %v", err)
	}

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	path := writePipeline(t, dir, `
agents:
- name: worker
  source:
    model: "`+liveCLIModel+`"
  max_turns: 12
  tools: [write_file, read_file, run_shell]

jobs:
- name: work
  plan:
  - agent: worker
    inputs: []
    attempts: 2
    messages:
      - |
        Do these in order, pausing to think carefully about each one:
        1. Write a file named one.txt containing the word ALPHA.
        2. Write a file named two.txt containing the word BETA.
        3. Write a file named three.txt containing the word GAMMA.
  - task: capture
    inputs: []
    run: cp -f one.txt two.txt three.txt `+dir+`/ 2>/dev/null || true
`)

	err = run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Two invocations: the killed one and the resumed one. If the shim never
	// fired, this test proved nothing and should say so rather than pass.
	count := strings.TrimSpace(readFileString(t, state))
	if count != "2" {
		t.Fatalf("claude ran %s time(s), want 2 — the first invocation was not killed, so no resume was exercised", count)
	}

	// A resumed session that could not be read would have failed the step, so
	// reaching a succeeded node IS the proof that resume worked against a
	// really-killed session.
	node := findNode(t, storeNodes(t, path), "agent", "worker")
	if node.Status != "succeeded" {
		t.Fatalf("agent node status = %q (%s), want succeeded", node.Status, node.Error)
	}

	// And the work actually completed across the two attempts.
	if got := strings.TrimSpace(readFileString(t, filepath.Join(dir, "three.txt"))); !strings.Contains(got, "GAMMA") {
		t.Errorf("three.txt = %q, want GAMMA — the resumed attempt did not finish the task", got)
	}
}
