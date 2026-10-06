package pipeline

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// TestBlockNodesRecordWhatTheBlockConcluded: a container's own node row is what `steps runs` and the node page read, so its status has to be the block's outcome rather than a constant.
func TestBlockNodesRecordWhatTheBlockConcluded(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		kind        merkle.NodeKind
		plan        string
		wantFailure bool
		want        string
	}{
		"in_parallel passing": {merkle.NodeKindParallel, `
  - in_parallel:
      steps:
      - task: a
        run: "true"`, false, "succeeded"},
		"in_parallel failing": {merkle.NodeKindParallel, `
  - in_parallel:
      steps:
      - task: a
        run: "false"`, true, "failed"},
		"race passing": {merkle.NodeKindRace, `
  - race:
      steps:
      - task: a
        run: "true"
      - task: b
        run: "true"`, false, "succeeded"},
		"race failing": {merkle.NodeKindRace, `
  - race:
      steps:
      - task: a
        run: "false"
      - task: b
        run: "false"`, true, "failed"},
		"across passing": {merkle.NodeKindAcross, `
  - across:
    - var: x
      values: [one, two]
    task: cell
    run: "true"`, false, "succeeded"},
		"across failing": {merkle.NodeKindAcross, `
  - across:
    - var: x
      values: [one, two]
    task: cell
    run: "false"`, true, "failed"},
		"try around a pass": {merkle.NodeKindTry, `
  - try:
      task: a
      run: "true"`, false, "succeeded"},
	} {
		_, st := runFixtureBeside(t, "jobs:\n- name: build\n  plan:"+c.plan+"\n", c.wantFailure, nil)

		if got := blockNode(t, st, c.kind).Status; got != c.want {
			t.Errorf("%s: %s node recorded %q, want %q", name, c.kind, got, c.want)
		}
	}
}

// TestInParallelClassifiesByItsBranches: branches that only said no make a step-level failure (on_failure), and a branch whose machinery broke makes an error (on_error).
func TestInParallelClassifiesByItsBranches(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		branch string
		want   string
	}{
		"a branch said no": {`
      - task: a
        run: "false"`, "failed"},
		"a branch could not run": {`
      - load_var: version
        file: out/missing.txt
        inputs: [out]`, "errored"},
	} {
		collected := runFixturePipeline(t, `
jobs:
- name: build
  plan:
  - task: make
    outputs: [out]
    run: "true"
  - in_parallel:
      steps:`+c.branch+`
      - task: b
        run: "true"
`, true)

		if got := blockFinish(t, collected, "in_parallel").Status; got != c.want {
			t.Errorf("%s: in_parallel finished %q, want %q", name, got, c.want)
		}
	}
}

// TestDoChildrenChainOneUnderTheNext: a do: block's chain reads do -> child -> child, so the second child is recorded under the first rather than beside it.
func TestDoChildrenChainOneUnderTheNext(t *testing.T) {
	t.Parallel()

	collected, st := runFixtureBeside(t, `
jobs:
- name: build
  plan:
  - do:
    - task: first
      run: "true"
    - task: second
      run: "true"
`, false, nil)

	first := findStepEvent(collected, events.TypeStepFinished, "task", "first")
	second := findStepEvent(collected, events.TypeStepFinished, "task", "second")

	if first == nil || second == nil || first.Hash == "" || second.Hash == "" {
		t.Fatalf("want both children finished with a hash, got first=%+v second=%+v", first, second)
	}

	nodes, err := st.NodesByHash(context.Background(), []string{second.Hash})
	if err != nil {
		t.Fatal(err)
	}

	if got := nodes[second.Hash].ParentHash; got != first.Hash {
		t.Errorf("second child recorded under %q, want the first child's %q", got, first.Hash)
	}
}

func blockFinish(t *testing.T, collected []events.Event, kind string) events.Event {
	t.Helper()

	for _, event := range collected {
		if event.Type == events.TypeStepFinished && event.StepKind == kind {
			return event
		}
	}

	t.Fatalf("no %s block published a finish", kind)

	return events.Event{}
}

func blockNode(t *testing.T, st store.Store, kind merkle.NodeKind) store.NodeRow {
	t.Helper()

	rows, err := st.ListNodes(context.Background(), "build", 100)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range rows {
		if row.Kind == string(kind) {
			return row
		}
	}

	t.Fatalf("no %s node recorded among %+v", kind, rows)

	return store.NodeRow{}
}
