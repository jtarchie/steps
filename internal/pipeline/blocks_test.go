package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/testdocker"
	"github.com/jtarchie/steps/internal/workspace"
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
		"do failing": {merkle.NodeKindDo, `
  - do:
    - task: a
      run: "false"`, true, "failed"},
		"do erroring": {merkle.NodeKindDo, `
  - do:
    - load_var: version
      file: out/missing.txt
      inputs: [out]`, true, "errored"},
		"in_parallel erroring": {merkle.NodeKindParallel, `
  - in_parallel:
      steps:
      - load_var: version
        file: out/missing.txt
        inputs: [out]`, true, "errored"},
		"race erroring": {merkle.NodeKindRace, `
  - race:
      steps:
      - load_var: version
        file: out/missing.txt
        inputs: [out]
      - load_var: other
        file: out/missing.txt
        inputs: [out]`, true, "errored"},
	} {
		_, st := runFixtureBeside(t, "jobs:\n- name: build\n  plan:\n  - task: make\n    outputs: [out]\n    run: \"true\""+c.plan+"\n", c.wantFailure, nil)

		if got := blockNode(t, st, c.kind).Status; string(got) != c.want {
			t.Errorf("%s: %s node recorded %q, want %q", name, c.kind, got, c.want)
		}
	}
}

// cancelOnceLive cancels the run once its block is observably running — the task has written its marker, or the approval is pending — because a fixed delay loses the race on a loaded machine and cancels before the block exists.
func cancelOnceLive(ctx context.Context, cancel context.CancelFunc, st store.Store, kind merkle.NodeKind, marker string) {
	live := func() bool {
		if kind == merkle.NodeKindApproval {
			pending, err := st.Approvals(ctx, true, 1)

			return err == nil && len(pending) > 0
		}

		_, err := os.Stat(marker)

		return err == nil
	}

	for !live() && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
}

// TestBlockNodesRecordAnAbort: a block stopped by the job's cancellation is aborted in the store, like the leaf inside it, not failed.
func TestBlockNodesRecordAnAbort(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		kind merkle.NodeKind
		plan string
	}{
		"do": {merkle.NodeKindDo, `
  - do:
    - task: a
      run: touch 'MARKER'; sleep 30`},
		"in_parallel": {merkle.NodeKindParallel, `
  - in_parallel:
      steps:
      - task: a
        run: touch 'MARKER'; sleep 30`},
		"try": {merkle.NodeKindTry, `
  - try:
      task: a
      run: touch 'MARKER'; sleep 30`},
		"approval": {merkle.NodeKindApproval, `
  - approval:
      message: ship it?`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.yml")

			marker := filepath.Join(dir, "started")
			plan := strings.ReplaceAll(c.plan, "MARKER", marker)

			err := os.WriteFile(path, []byte("jobs:\n- name: build\n  plan:"+plan+"\n"), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}

			st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })

			provider, err := workspace.NewProvider(nil, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			go cancelOnceLive(ctx, cancel, st, c.kind, marker)

			if RunJob(ctx, cfg, &cfg.Jobs[0], st, Run{Provider: provider}) == nil {
				t.Fatal("RunJob returned nil for a cancelled run")
			}

			if got := blockNode(t, st, c.kind).Status; got != "aborted" {
				t.Errorf("%s node recorded %q, want aborted", c.kind, got)
			}
		})
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
		"one said no and another could not run": {`
      - task: a
        run: "false"
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

// Like in_parallel, a matrix takes its worst cell's class: one cell saying no and another unable to run is errored, so on_error fires rather than on_failure, which is Concourse's answer for across too. The errored cell's image swallows the keepalive, so its container dies at birth — caught only inside the settle window, which a loaded daemon outlasts at 300ms and the cell then reads failed.
func TestAcrossClassifiesByItsWorstCell(t *testing.T) {
	testdocker.Require(t)
	t.Cleanup(shell.WidenSettleBoundForTest(5 * time.Second))

	dies := "steps-test-dies-at-birth:" + strconv.Itoa(os.Getpid())

	build := exec.CommandContext(t.Context(), "docker", "build", "-q", "-t", dies, "-") //nolint:gosec // a tag this test made
	build.Stdin = strings.NewReader("FROM alpine:3\nENTRYPOINT [\"/bin/echo\"]\n")

	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("building the fixture image: %v\n%s", err, out)
	}

	t.Cleanup(func() { _ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "rmi", "-f", dies).Run() }) //nolint:gosec // as above

	for _, inFlight := range []string{"", "\n    max_in_flight: 2"} {
		collected := runFixturePipeline(t, `
jobs:
- name: build
  plan:
  - across:
    - var: image
      values: ["alpine:3", "`+dies+`"]`+inFlight+`
    task: check
    image: "{{ .vars.image }}"
    run: "false"
`, true)

		statuses := map[string]int{}
		for _, event := range collected {
			if event.Type == events.TypeStepFinished && event.StepKind == "task" {
				statuses[event.Status]++
			}
		}

		if statuses["failed"] != 1 || statuses["errored"] != 1 {
			t.Fatalf("max_in_flight %q: cells finished %v, want one failed and one errored, or this proves nothing", inFlight, statuses)
		}

		if got := blockFinish(t, collected, "across").Status; got != "errored" {
			t.Errorf("max_in_flight %q: across finished %q, want errored", inFlight, got)
		}
	}
}
