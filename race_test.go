package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestRaceKeepsTheFirstSuccess is the feature in one assertion: the fast
// branch's result is the step's result, and the slow one is cancelled rather
// than waited on.
func TestRaceKeepsTheFirstSuccess(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - race:
      steps:
      - task: fast
        inputs: []
        run: echo fast
      - task: slow
        inputs: []
        run: sleep 5; echo ran >> %s
`, slow))

	mustRun(t, path)

	// The slow branch never got to write: it was cancelled when the fast one
	// won. Without cancellation this test would take five seconds and the
	// file would exist.
	assertNoFile(t, slow)
}

// TestRaceIgnoresAFailingBranch verifies "first SUCCESS wins", not "first to
// finish wins" — a branch that fails fast must not decide the block.
func TestRaceIgnoresAFailingBranch(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - race:
      steps:
      - task: quick-failure
        inputs: []
        run: exit 1
      - task: slower-success
        inputs: []
        run: sleep 0.3; echo ok
`)

	mustRun(t, path)
}

// TestRaceFailsWhenEveryBranchFails verifies the block reports every failure
// when nothing won — a race is a hedge, not a guarantee.
func TestRaceFailsWhenEveryBranchFails(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - race:
      steps:
      - task: alpha
        inputs: []
        run: exit 1
      - task: beta
        inputs: []
        run: exit 1
`)

	err := run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("the race succeeded with every branch failing")
	}

	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name branch %q: %v", want, err)
		}
	}
}

// TestRaceWinnersOutputsReachTheNextStep pins the contract downstream steps
// depend on: the winner's outputs are the step's outputs, and a later step
// consumes them without knowing which branch produced them.
func TestRaceWinnersOutputsReachTheNextStep(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - race:
      steps:
      - task: fast
        inputs: []
        outputs: [summary]
        run: mkdir -p summary && echo from-fast > summary/text
      - task: slow
        inputs: []
        outputs: [summary]
        run: sleep 5; mkdir -p summary && echo from-slow > summary/text
  - task: consumer
    inputs: [summary]
    run: cat summary/text >> %s
`, marker))

	mustRun(t, path)

	if got := readFileString(t, marker); !strings.Contains(got, "from-fast") {
		t.Errorf("downstream step saw %q, want the winner's output", got)
	}
}
