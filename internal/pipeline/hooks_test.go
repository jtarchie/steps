package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/workspace"
)

// hookTestScope builds a hookScope backed by a real shared build workspace and
// a config whose named tasks each append a marker line to markerFile, so a
// test can assert which hooks fired and in what order.
func hookTestScope(t *testing.T, markerFile string) hookScope {
	t.Helper()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { workspace.CloseBuild(bw, "test") })

	cfg := &config.Config{
		Tasks: []config.Task{
			{Name: "on_success", Run: "echo on_success >> " + markerFile},
			{Name: "on_failure", Run: "echo on_failure >> " + markerFile},
			{Name: "on_error", Run: "echo on_error >> " + markerFile},
			{Name: "on_abort", Run: "echo on_abort >> " + markerFile},
			{Name: "ensure", Run: "echo ensure >> " + markerFile},
		},
	}

	return stepRunner{cfg: cfg, jobName: "test", bw: bw}.scope("step 0")
}

func readMarkers(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp file
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Fields(strings.TrimSpace(string(data)))
}

// allHooks wires every task hook by its own name, so a marker line names which
// hook ran.
func allHooks() config.Hooks {
	return config.Hooks{
		OnSuccess: &config.Step{Task: "on_success"},
		OnFailure: &config.Step{Task: "on_failure"},
		OnError:   &config.Step{Task: "on_error"},
		OnAbort:   &config.Step{Task: "on_abort"},
		Ensure:    &config.Step{Task: "ensure"},
	}
}

// TestConformance note: verifies on_success/on_failure/on_error firing
// conditions against Concourse's own doc (concourse-ci.org/docs/steps/):
// on_success on a nil error, on_failure on a task-level Failure (outcome.Fail
// — Concourse: "the parent step fails" but "does not recover the failure"),
// on_error on any other error (Concourse: "terminates abnormally in any way
// other than those handled by on_abort or on_failure" — matches
// outcome.Errored's "infrastructure error" bucket exactly). See
// docs/conformance.md.
func TestRunHooksRouting(t *testing.T) {
	tests := []struct {
		name    string
		baseErr error
		want    []string
	}{
		{"success runs on_success then ensure", nil, []string{"on_success", "ensure"}},
		{"marked failure runs on_failure then ensure", outcome.Fail(errors.New("exit 1")), []string{"on_failure", "ensure"}},
		{"plain error runs on_error then ensure", errors.New("infra down"), []string{"on_error", "ensure"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "markers.txt")
			scope := hookTestScope(t, marker)

			err := runHooks(context.Background(), scope, allHooks(), tt.baseErr)

			// A non-nil base error must propagate unchanged (observer semantics).
			if tt.baseErr != nil && err == nil {
				t.Error("runHooks consumed the base error; want it propagated")
			}

			if tt.baseErr == nil && err != nil {
				t.Errorf("runHooks returned %v on a green outcome", err)
			}

			got := readMarkers(t, marker)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hooks fired = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunHooksAbortGracePeriod verifies on_abort and ensure both run to
// completion even when the job context is already canceled — they run detached
// under the grace period.
//
// TestConformance note: verifies on_abort fires on a canceled context and
// ensure fires "regardless of whether the parent step succeeds, fails, or
// errors... also executed if the build was aborted," per
// concourse-ci.org/docs/steps/. See docs/conformance.md.
func TestRunHooksAbortGracePeriod(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "markers.txt")
	scope := hookTestScope(t, marker)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = runHooks(ctx, scope, allHooks(), errors.New("interrupted"))

	got := readMarkers(t, marker)
	want := []string{"on_abort", "ensure"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hooks fired under canceled ctx = %v, want %v", got, want)
	}
}

// TestRunHooksEnsureFailsGreenOutcome verifies a failing ensure hook turns a
// green outcome into a failure, while a failing ensure on an already-failing
// outcome is swallowed.
func TestRunHooksEnsureFailsGreenOutcome(t *testing.T) {
	cfg := &config.Config{Tasks: []config.Task{{Name: "boom", Run: "exit 1"}}}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	newScope := func() hookScope {
		bw, err := provider.NewBuild(context.Background(), "test")
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { workspace.CloseBuild(bw, "test") })

		return stepRunner{cfg: cfg, jobName: "test", bw: bw}.scope("step 0")
	}

	hooks := config.Hooks{Ensure: &config.Step{Task: "boom"}}

	// Green base: a failing ensure makes it fail.
	greenErr := runHooks(context.Background(), newScope(), hooks, nil)
	if greenErr == nil {
		t.Error("a failing ensure hook did not fail a green outcome")
	}

	// Failing base: a failing ensure is swallowed, base error returned.
	base := outcome.Fail(errors.New("original"))

	failErr := runHooks(context.Background(), newScope(), hooks, base)
	if !errors.Is(failErr, base) {
		t.Errorf("runHooks returned %v, want the original base error", failErr)
	}
}

// hookRow is what one hook's row published: its start, its outputs and its
// finish, all found by the id the start minted.
type hookRow struct {
	started  events.Event
	outputs  []events.Event
	finished events.Event
}

// hookRowNamed finds the hook row whose name starts with prefix, and fails
// when there is none or its events disagree on who it is.
func hookRowNamed(t *testing.T, collected []events.Event, prefix string) hookRow {
	t.Helper()

	row := hookRow{started: hookStarted(t, collected, prefix)}

	for _, event := range collected {
		if event.StepID != row.started.StepID {
			continue
		}

		switch event.Type {
		case events.TypeStepOutput:
			row.outputs = append(row.outputs, event)
		case events.TypeStepFinished:
			row.finished = event
		}
	}

	if row.finished.Type == "" {
		t.Fatalf("hook row %q never finished — it would read as running forever", row.started.StepName)
	}

	if row.finished.StepKind != "hook" || row.finished.ParentStepID != row.started.ParentStepID {
		t.Errorf("finish = %+v, want kind hook under the start's parent %d", row.finished, row.started.ParentStepID)
	}

	return row
}

func hookStarted(t *testing.T, collected []events.Event, prefix string) events.Event {
	t.Helper()

	for _, event := range collected {
		if event.Type == events.TypeStepStarted && event.StepKind == "hook" && strings.HasPrefix(event.StepName, prefix) {
			return event
		}
	}

	t.Fatalf("no hook row named %q* was started; events: %+v", prefix, collected)

	return events.Event{}
}

func stepStartedNamed(t *testing.T, collected []events.Event, kind, name string) events.Event {
	t.Helper()

	for _, event := range collected {
		if event.Type == events.TypeStepStarted && event.StepKind == kind && event.StepName == name {
			return event
		}
	}

	t.Fatalf("no %s %q started", kind, name)

	return events.Event{}
}

func TestJobHookPublishesItsOwnRow(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: compile
        run: exit 1
    on_failure:
      task: explain-failure
      run: echo see steps runs
`, true)

	row := hookRowNamed(t, collected, "on_failure")

	if row.started.StepName != "on_failure · task explain-failure" {
		t.Errorf("hook row name = %q, want %q", row.started.StepName, "on_failure · task explain-failure")
	}

	if row.started.ParentStepID != 0 {
		t.Errorf("job hook parent = %d, want 0 (a root after the plan)", row.started.ParentStepID)
	}

	if len(row.outputs) != 1 || !strings.Contains(row.outputs[0].Text, "see steps runs") {
		t.Errorf("hook row outputs = %+v, want the hook's echo", row.outputs)
	}

	if row.outputs[0].StepName != row.started.StepName || row.outputs[0].StepKind != "hook" {
		t.Errorf("output labelled %s %q, want the hook row's own label", row.outputs[0].StepKind, row.outputs[0].StepName)
	}

	if row.finished.Status != "succeeded" {
		t.Errorf("hook finished %q, want succeeded", row.finished.Status)
	}
}

func TestStepHookIsAChildOfItsStep(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: compile
        run: exit 1
        on_failure:
          task: tell
          run: echo step hook ran
`, true)

	compile := stepStartedNamed(t, collected, "task", "compile")
	row := hookRowNamed(t, collected, "on_failure")

	if row.started.ParentStepID != compile.StepID {
		t.Errorf("step hook parent = %d, want the guarded step's id %d", row.started.ParentStepID, compile.StepID)
	}

	if len(row.outputs) != 1 || !strings.Contains(row.outputs[0].Text, "step hook ran") {
		t.Errorf("hook outputs = %+v, want the hook's echo on the hook's own row", row.outputs)
	}

	for _, event := range collected {
		if event.Type == events.TypeStepOutput && event.StepID == compile.StepID && strings.Contains(event.Text, "step hook ran") {
			t.Errorf("the hook's output was filed under the step it guards: %+v", event)
		}
	}
}

// An in-place get runs its hooks under its own row. Without scoping the
// children there, they took the enclosing build as their parent and drew as
// the get's siblings.
func TestInPlaceGetHookIsAChildOfTheGet(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
resource_types:
  - name: dummy
    config:
      check: 'echo ''[{"ref":"v1"}]'''
      in: "true"

resources:
  - name: alpha
    type: dummy
    source: {key: a}
  - name: beta
    type: dummy
    source: {key: b}

jobs:
  - name: build
    plan:
      - get: alpha
        trigger: true
      - get: beta
        on_success:
          task: tell
          run: echo fetched
`, false)

	beta := stepStartedNamed(t, collected, "get", "beta")
	row := hookRowNamed(t, collected, "on_success")

	if row.started.ParentStepID != beta.StepID {
		t.Errorf("in-place get hook parent = %d, want the get's id %d", row.started.ParentStepID, beta.StepID)
	}
}

// A failing hook used to reach only the log. Its row finishes red, with the
// reason on it.
func TestFailingHookFinishesWithItsReason(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: compile
        run: exit 1
    on_failure:
      task: broken
      run: exit 3
`, true)

	row := hookRowNamed(t, collected, "on_failure")

	if row.finished.Status != "failed" || !strings.Contains(row.finished.Text, "exit status 3") {
		t.Errorf("failing hook finished %q with text %q, want failed with the reason", row.finished.Status, row.finished.Text)
	}
}

func TestHookOfAHookIsItsChild(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: compile
        run: exit 1
    on_failure:
      task: explain
      run: "true"
      ensure:
        task: cleanup
        run: echo cleaned
`, true)

	outer := hookRowNamed(t, collected, "on_failure")
	inner := hookRowNamed(t, collected, "ensure")

	if inner.started.ParentStepID != outer.started.StepID {
		t.Errorf("nested hook parent = %d, want the outer hook's id %d", inner.started.ParentStepID, outer.started.StepID)
	}
}
