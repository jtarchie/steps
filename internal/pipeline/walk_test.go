package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// max_visits counts the times a step RAN: a pass where its when: guard skipped it executed nothing, so it must not spend the loop's budget.
func TestAGuardSkippedPassIsNotAVisit(t *testing.T) {
	t.Parallel()

	counter := filepath.Join(t.TempDir(), "counter")

	runFixturePipeline(t, `
jobs:
- name: build
  plan:
  - task: a
    run: echo x >> `+counter+`
  - task: gated
    when: test $(wc -l < `+counter+`) -ge 2
    run: test $(wc -l < `+counter+`) -ge 3
    to: { failure: a }
    max_visits: 2
  - task: z
    run: test $(wc -l < `+counter+`) -ge 2
    to: { failure: a }
    max_visits: 3
`, false)
}

// A green step its own on_success hook failed is a failure like any other: the step a to: route then carries on to chains under the parent the failed step ran beneath, not under the result the hook rejected.
func TestAStepItsHookFailedDoesNotParentTheRoute(t *testing.T) {
	t.Parallel()

	collected, st := runFixtureBeside(t, `
jobs:
- name: build
  plan:
  - task: first
    run: "true"
  - task: work
    run: "true"
    on_success:
      task: reject
      run: exit 1
    to: {failure: recover}
  - task: recover
    run: "true"
`, false, nil)

	first := findStepEvent(collected, events.TypeStepFinished, "task", "first")
	recovered := findStepEvent(collected, events.TypeStepFinished, "task", "recover")

	if first == nil || recovered == nil || first.Hash == "" {
		t.Fatalf("missing step_finished: first=%v recover=%v", first, recovered)
	}

	nodes, err := st.NodesByHash(context.Background(), []string{recovered.Hash})
	if err != nil {
		t.Fatal(err)
	}

	if got := nodes[recovered.Hash].ParentHash; got != first.Hash {
		t.Errorf("recover chained under %q, want %q (the step before the one its hook failed)", got, first.Hash)
	}
}

// A step a chain skip swallowed is published, and logged, at the position it holds in the plan — the transcript and a daemon's log both place it by that index.
func TestChainSkippedStepsKeepTheirPlanIndex(t *testing.T) {
	run := fixtureRunner(t, `
jobs:
- name: build
  plan:
  - task: prep
    run: "true"
  - task: compile
    run: "true"
  - task: package
    run: "true"
`)

	ran := run()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	replayed := run()

	logged := map[string]string{}

	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "job.skip") && logField(line, "reason") == "chain" {
			logged[logField(line, "step")] = logField(line, "index")
		}
	}

	for _, name := range []string{"compile", "package"} {
		finished := findStepEvent(ran, events.TypeStepFinished, "task", name)
		skipped := findStepEvent(replayed, events.TypeStepSkipped, "task", name)

		if finished == nil || skipped == nil {
			t.Fatalf("%s: finished=%v skipped=%v, want a run then a replay", name, finished, skipped)
		}

		if skipped.StepIndex != finished.StepIndex {
			t.Errorf("%s replayed at index %d, want the index it ran at, %d", name, skipped.StepIndex, finished.StepIndex)
		}

		if want := strconv.Itoa(finished.StepIndex); logged[name] != want {
			t.Errorf("%s logged its chain skip at index %q, want %q", name, logged[name], want)
		}
	}
}
