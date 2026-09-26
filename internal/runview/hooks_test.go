package runview

import (
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// foldEvents builds a transcript from hand-written rows, numbering them.
func foldEvents(rows ...store.RunEventRow) Transcript {
	for i := range rows {
		rows[i].Seq = int64(i + 1)
	}

	folder := NewFolder()
	folder.Add(rows, nil)

	return folder.View(store.RunRow{ID: "R"})
}

func started(id, parent int64, kind, name string) store.RunEventRow {
	return store.RunEventRow{Type: events.TypeStepStarted, StepID: id, ParentStepID: parent, StepKind: kind, StepName: name}
}

func finished(id, parent int64, kind, name, status string) store.RunEventRow {
	return store.RunEventRow{Type: events.TypeStepFinished, StepID: id, ParentStepID: parent, StepKind: kind, StepName: name, Status: status}
}

func TestInnermostFailureAndHooks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []store.RunEventRow
		want string
	}{
		{
			// The hook reacted to the step's failure; it did not cause it.
			name: "a failed step's failed hook is not the cause",
			rows: []store.RunEventRow{
				started(1, 0, "task", "build"),
				finished(1, 0, "task", "build", "failed"),
				started(2, 1, "hook", "on_failure · task page"),
				finished(2, 1, "hook", "on_failure · task page", "failed"),
			},
			want: "build",
		},
		{
			// A promoted on_success: the step's row stays green, the job is red.
			name: "a green step's failed hook is the cause",
			rows: []store.RunEventRow{
				started(1, 0, "task", "build"),
				finished(1, 0, "task", "build", "succeeded"),
				started(2, 1, "hook", "on_success · task notify"),
				finished(2, 1, "hook", "on_success · task notify", "failed"),
			},
			want: "on_success · task notify",
		},
		{
			name: "a failed job hook after a green plan",
			rows: []store.RunEventRow{
				started(1, 0, "task", "build"),
				finished(1, 0, "task", "build", "succeeded"),
				started(2, 0, "hook", "ensure · task cleanup"),
				finished(2, 0, "hook", "ensure · task cleanup", "failed"),
			},
			want: "ensure · task cleanup",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := foldEvents(tc.rows...).InnermostFailure()
			if got == nil || got.Name != tc.want {
				t.Errorf("InnermostFailure = %+v, want %q", got, tc.want)
			}
		})
	}
}

// A task with hooks is still a task: no rollup counting the hooks as its
// steps, and not drawn open unless something in it went wrong.
func TestHooksDoNotMakeAStepABlock(t *testing.T) {
	t.Parallel()

	view := foldEvents(
		started(1, 0, "task", "build"),
		finished(1, 0, "task", "build", "succeeded"),
		started(2, 1, "hook", "on_success"),
		finished(2, 1, "hook", "on_success", "succeeded"),
		started(3, 1, "hook", "ensure"),
		finished(3, 1, "hook", "ensure", "succeeded"),
	)

	build := view.Roots[0]

	if rollup := build.Rollup(); !rollup.Empty() || rollup.Cells != 0 {
		t.Errorf("rollup = %+v, want nothing: hooks are not cells", rollup)
	}

	if build.OpenByDefault() {
		t.Error("a green task with passing hooks opens by default")
	}

	if build.Block() || !build.Container() {
		t.Errorf("Block=%v Container=%v, want a container that is not a block", build.Block(), build.Container())
	}

	failedHook := foldEvents(
		started(1, 0, "task", "build"),
		finished(1, 0, "task", "build", "succeeded"),
		started(2, 1, "hook", "ensure"),
		finished(2, 1, "hook", "ensure", "failed"),
	)

	if !failedHook.Roots[0].OpenByDefault() {
		t.Error("a task whose hook failed does not open by default")
	}
}

// A step's hooks run after it has finished; the rail must follow them.
func TestAFinishedStepWithARunningHookIsActive(t *testing.T) {
	t.Parallel()

	view := foldEvents(
		started(1, 0, "task", "build"),
		finished(1, 0, "task", "build", "failed"),
		started(2, 1, "hook", "on_failure"),
	)

	if !view.Roots[0].Active() {
		t.Error("a finished step whose hook is running is not active")
	}
}

func TestLiveDrawsARunningHookUnderAFinishedStep(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	for _, e := range []events.Event{
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "build", StepID: 1},
		{Type: events.TypeStepFinished, StepKind: "task", StepName: "build", StepID: 1, Status: "failed"},
		{Type: events.TypeStepStarted, StepKind: "hook", StepName: "on_failure · task page", StepID: 2, ParentStepID: 1},
	} {
		e.RunID, e.Job, e.At = "R", "build", now
		live.Event(e)
	}

	got := strings.Join(screen(t, out.String()), "\n")
	if !strings.Contains(got, "▸ hook on_failure · task page") {
		t.Errorf("the running hook is not drawn:\n%s", got)
	}

	live.Stop()
}

// A failed task keeps its output in the summary although its hook made it a
// container, and the hook's own output follows: on a failed run, that is the
// explanation it printed.
func TestLiveSummaryKeepsAHookedFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	for _, e := range []events.Event{
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "lint", StepID: 1},
		{Type: events.TypeStepOutput, StepKind: "task", StepName: "lint", StepID: 1, Text: "a.go:1 unused"},
		{Type: events.TypeStepFinished, StepKind: "task", StepName: "lint", StepID: 1, Status: "failed"},
		{Type: events.TypeStepStarted, StepKind: "hook", StepName: "on_failure · task page", StepID: 2, ParentStepID: 1},
		{Type: events.TypeStepOutput, StepKind: "hook", StepName: "on_failure · task page", StepID: 2, Text: "see steps runs"},
		{Type: events.TypeStepFinished, StepKind: "hook", StepName: "on_failure · task page", StepID: 2, ParentStepID: 1, Status: "succeeded"},
		{Type: events.TypeJobFinished, StepIndex: -1, Status: "failed"},
	} {
		e.RunID, e.Job, e.At = "R", "build", now
		live.Event(e)
	}

	got := strings.Join(screen(t, out.String()), "\n")

	for _, want := range []string{"── task lint ──\na.go:1 unused", "── hook on_failure · task page ──\nsee steps runs"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}
