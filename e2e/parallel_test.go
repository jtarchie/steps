package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// interleavePipeline builds a job whose branches each announce when they start
// and when they finish, with a sleep between. The ORDER of those announcements
// is what proves concurrency, without measuring wall-clock time — a timing
// assertion would flake under load, which is exactly when a CI box is busy.
func interleavePipeline(t *testing.T, dir, block string) string {
	t.Helper()

	log := filepath.Join(dir, "order.log")

	return writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - in_parallel:
%[1]s      steps:
      - task: one
        inputs: []
        run: echo start-one >> %[2]s; sleep 0.3; echo end-one >> %[2]s
      - task: two
        inputs: []
        run: echo start-two >> %[2]s; sleep 0.3; echo end-two >> %[2]s
`, block, log))
}

// orderLines reads the announcements a run produced.
func orderLines(t *testing.T, dir string) []string {
	t.Helper()

	return strings.Fields(readFileString(t, filepath.Join(dir, "order.log")))
}

// TestInParallelActuallyOverlaps is the feature's whole premise: the branches
// run at the same time. Both starts must land before either end — under
// sequential execution the first branch's end would come before the second's
// start, whatever the machine's speed.
func TestInParallelActuallyOverlaps(t *testing.T) {
	dir := t.TempDir()

	mustRun(t, interleavePipeline(t, dir, ""))

	got := orderLines(t, dir)
	if len(got) != 4 {
		t.Fatalf("order = %v, want four entries", got)
	}

	if !strings.HasPrefix(got[0], "start-") || !strings.HasPrefix(got[1], "start-") {
		t.Errorf("order = %v, want both starts before either end — the branches did not overlap", got)
	}
}

// TestInParallelLimitOneIsSequential pins limit: from the other side. With one
// branch in flight at a time the interleaving must disappear entirely, which
// is a deterministic assertion rather than a timing one.
func TestInParallelLimitOneIsSequential(t *testing.T) {
	dir := t.TempDir()

	mustRun(t, interleavePipeline(t, dir, "      limit: 1\n"))

	got := orderLines(t, dir)
	if len(got) != 4 {
		t.Fatalf("order = %v, want four entries", got)
	}

	// start-X, end-X, start-Y, end-Y — never two starts in a row.
	if !strings.HasPrefix(got[1], "end-") || !strings.HasPrefix(got[2], "start-") {
		t.Errorf("order = %v, want strictly sequential under limit: 1", got)
	}
}

// TestInParallelFailFastCancelsSiblings verifies fail_fast: true stops work
// that has not finished, rather than merely reporting sooner.
func TestInParallelFailFastCancelsSiblings(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "slow.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - in_parallel:
      fail_fast: true
      limit: 1
      steps:
      - task: failer
        inputs: []
        run: exit 1
      - task: slow
        inputs: []
        run: echo ran >> %s
`, marker))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("job succeeded with a failing branch")
	}

	// limit: 1 makes the ordering deterministic: failer runs first and
	// cancels the block before slow is ever started.
	assertNoFile(t, marker)
}

// TestInParallelReportsEveryFailure verifies a reader debugging a parallel
// block is told whether one branch broke or all of them did. A report
// truncated at the first failure hides exactly that.
func TestInParallelReportsEveryFailure(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: alpha
        inputs: []
        run: exit 1
      - task: beta
        inputs: []
        run: exit 1
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("job succeeded with two failing branches")
	}

	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name branch %q: %v", want, err)
		}
	}
}

// TestInParallelBranchesCannotSeeSiblingOutputs pins the artifact rule.
// Concurrent branches have no order between them, so a branch consuming a
// sibling's output is a race — and the plan-time answer has to be "not
// available" rather than "sometimes".
func TestInParallelBranchesCannotSeeSiblingOutputs(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: producer
        inputs: []
        outputs: [report]
        run: mkdir -p report && echo hi > report/x
      - task: consumer
        inputs: [report]
        run: cat report/x
`)

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil {
		t.Fatal("a branch was allowed to consume a sibling branch's output")
	}

	if !strings.Contains(err.Error(), "not a resource fetched") {
		t.Errorf("error does not explain the artifact is unavailable: %v", err)
	}
}

// TestInParallelOutputsAreAvailableAfterTheBlock is the other half: what the
// branches produced IS available to the steps that follow, which is where
// consuming it is well-defined.
func TestInParallelOutputsAreAvailableAfterTheBlock(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: producer
        inputs: []
        outputs: [report]
        run: mkdir -p report && echo hi > report/x
  - task: consumer
    inputs: [report]
    run: cat report/x >> %s
`, marker))

	mustRun(t, path)

	assertLineCount(t, marker, 1)
}
