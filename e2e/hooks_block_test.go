package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/testsshd"
)

// A hook body can be a block, as in Concourse (#260): an on_failure: do: runs
// every child, in order, and the job still reports the step's failure.
func TestADoHookRunsEveryChild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	order := filepath.Join(dir, "order.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: "false"
    on_failure:
      do:
      - task: say-why
        run: echo say-why >> %[1]s
      - task: page
        run: echo page >> %[1]s
`, order))

	var err error

	out := captureStdout(t, func() { err = stepsRun(t, []string{path}) })
	if err == nil {
		t.Fatal("cli.Run succeeded, want the step's failure")
	}

	if got := readTrimmed(t, order); got != "say-why\npage" {
		t.Errorf("hook children ran %q, want say-why then page", got)
	}

	announced := strings.Index(out, "hook: on_failure")
	child := strings.Index(out, "say-why")

	if announced < 0 || child < announced {
		t.Errorf("want the hook row announced before its children:\n%s", out)
	}
}

// A hook stays off the merkle chain when its body is a block: its children
// record no node and fill no step cache, so a second run runs every one again.
// The child with outputs is the one the step cache would otherwise reuse: the
// cache lives under a workspace root, and keys on a get's bytes.
func TestABlockHookIsNeverCached(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	path := pipelinePath(t, dir)
	produced := filepath.Join(dir, "produced.txt")
	branches := filepath.Join(dir, "branches.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo hi > file

resources:
- name: repo
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: work
    run: "false"
    on_failure:
      do:
      - task: produce
        outputs: [report]
        run: echo produced >> %[1]s; echo x > report/out
      - in_parallel:
        - task: left
          run: echo left >> %[2]s
        - task: right
          run: echo right >> %[2]s
`, produced, branches))

	for range 2 {
		if stepsRun(t, []string{"--workspace-root", root, path}) == nil {
			t.Fatal("cli.Run succeeded, want the step's failure")
		}
	}

	assertLineCount(t, produced, 2)
	assertLineCount(t, branches, 4)

	st := openStoreFor(t, path)

	nodes, err := st.ListNodes(context.Background(), "build", 100)
	if err != nil {
		t.Fatal(err)
	}

	for _, node := range nodes {
		switch node.Resource {
		case "produce", "left", "right", "":
			t.Errorf("a hook body recorded node %s (%s %q), which a later run could skip on", node.Hash, node.Kind, node.Resource)
		}
	}
}

// A job-level in_parallel: hook runs every branch on every run, even when the
// plan itself is skipped as already done.
func TestAnInParallelJobHookRunsEveryBranchEveryRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	work := filepath.Join(dir, "work.txt")
	branches := filepath.Join(dir, "branches.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: echo work >> %[1]s
  ensure:
    in_parallel:
    - task: tidy
      run: echo tidy >> %[2]s
    - task: notify
      run: echo notify >> %[2]s
`, work, branches))

	mustRun(t, path)
	mustRun(t, path)

	assertLineCount(t, work, 1)
	assertLineCount(t, branches, 4)
}

// A race: hook keeps its first winner, as a race: in the plan does.
func TestARaceHookRunsItsBranches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	won := filepath.Join(dir, "won.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: "false"
  on_failure:
    race:
      steps:
      - task: broken
        run: "false"
      - task: works
        run: echo won >> %s
`, won))

	wantRunError(t, path)

	assertLineCount(t, won, 1)
}

// A step inside a hook body keeps its own hooks.
func TestAHookBodyChildFiresItsOwnHooks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	inner := filepath.Join(dir, "inner.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: "false"
  on_failure:
    do:
    - task: notify
      run: "false"
      on_failure:
        task: escalate
        run: echo escalated >> %s
`, inner))

	wantRunError(t, path)

	assertLineCount(t, inner, 1)
}

// A matrix hook runs every cell, every time — even a cell whose hash matches
// a plan cell that already succeeded. The plan's first step chains under no
// parent, and neither does a hook body, so the two collide by construction.
func TestAnAcrossHookRunsEveryCell(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	cells := filepath.Join(dir, "cells.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: shard
    across:
    - var: shard
      values: [a, b]
    run: echo {{ .vars.shard }} >> %[1]s
  ensure:
    task: shard
    across:
    - var: shard
      values: [a, b]
    run: echo {{ .vars.shard }} >> %[1]s
`, cells))

	mustRun(t, path)

	assertLineCount(t, cells, 4)
}

// Cells sharing a hash are asked about one at a time rather than prefetched;
// a hook's are told no either way. The plan runs its repeated cell once.
func TestAnAcrossHookRunsARepeatedCellEveryTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	cells := filepath.Join(dir, "cells.txt")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: shard
    across:
    - var: shard
      values: [a, a]
    run: echo {{ .vars.shard }} >> %[1]s
  ensure:
    task: shard
    across:
    - var: shard
      values: [a, a]
    run: echo {{ .vars.shard }} >> %[1]s
`, cells))

	mustRun(t, path)

	assertLineCount(t, cells, 3)
}

// A hook is part of the step it guards, so editing a matrix hook's axis — the
// one part of it no cell's own content names — re-runs the step.
func TestEditingAnAcrossHookRerunsItsStep(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	work := filepath.Join(dir, "work.txt")

	write := func(values string) {
		t.Helper()

		writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: work
    run: echo work >> %s
    ensure:
      task: shard
      across:
      - var: shard
        values: [%s]
      run: echo {{ .vars.shard }}
`, work, values))
	}

	write("a")
	mustRun(t, path)

	write("a, b")
	mustRun(t, path)

	assertLineCount(t, work, 2)
}

// A tagged step inside a block hook body records the machine it ran on, under
// no node and keyed on its hook: two hooks holding the same body are two
// machines, not one row upserted over the other.
func TestATaggedStepInABlockHookRecordsItsMachine(t *testing.T) {
	t.Parallel()

	worker := testsshd.New(t).URL

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: here
    run: "false"
    on_failure:
      do:
      - task: notify
        tags: [gpu]
        run: "true"
    ensure:
      do:
      - task: notify
        tags: [gpu]
        run: "true"
`)

	_ = stepsRun(t, []string{path, "--worker", "gpu=" + worker})

	placements := runPlacements(t, path)

	slots := map[string]bool{}

	for _, placement := range placements {
		slots[placement.Slot] = true

		if placement.NodeHash != "" {
			t.Errorf("placement %q names node %s, which a hook body never records", placement.Slot, placement.NodeHash)
		}
	}

	for _, want := range []string{"on_failure", "ensure"} {
		found := false

		for slot := range slots {
			found = found || strings.Contains(slot, want)
		}

		if !found {
			t.Errorf("no placement slot names the %s hook; slots are %v", want, slots)
		}
	}
}

// An agent inside a block hook body spends like a hook's own agent: its tokens
// are the run's, counted as a hook's, drawn on the agent's own row under the
// hook — with no node for the ledger or a transcript to hang on.
func TestAnAgentInABlockHookIsCountedAsAHooksSpend(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("noted").spending(7000))
	stderr := captureStderr(t)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: notifier
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - task: work
    run: echo did the work
    on_success:
      do:
      - task: tidy
        run: echo tidy
      - agent: notifier
        messages:
          - Say something.
`, fake.URL))

	mustRun(t, "run", path, "--job", "build")

	assertRunCostShows(t, path, "notifier (hook)", "7,000", 7000)

	if logged := stderr(); strings.Contains(logged, "transcript_save") || strings.Contains(logged, "RecordPlacement") {
		t.Errorf("a hook body's agent wrote a record that names a node it never had:\n%s", logged)
	}

	body := latestRunPage(t, path)

	hook := subtreeOf(t, body, "hook")
	if !strings.Contains(hook, "tidy") || !strings.Contains(hook, "notifier") {
		t.Errorf("the hook's row does not hold its body's steps:\n%s", hook)
	}

	if strings.Contains(hook, "/nodes/") {
		t.Errorf("a hook body's step links to a node it never recorded:\n%s", hook)
	}

	if agent := headOf(t, body, "agent"); !strings.Contains(agent, "7,000 tokens") {
		t.Errorf("the agent's row does not show its 7,000 tokens:\n%s", agent)
	}
}
