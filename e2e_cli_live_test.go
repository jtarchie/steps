package main

import (
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
    prompt: Create a file named out.txt containing exactly the word WROTE. Then stop.
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
    prompt: |
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
