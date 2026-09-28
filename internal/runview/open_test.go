package runview

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

func turn(id int64) store.RunEventRow {
	return store.RunEventRow{Type: events.TypeAgentText, StepID: id, Text: "thinking"}
}

// foldEnded is foldEvents on a run that has finished, where a step with no
// close event reads unreported rather than running.
func foldEnded(rows ...store.RunEventRow) Transcript {
	for i := range rows {
		rows[i].Seq = int64(i + 1)
	}

	folder := NewFolder()
	folder.Add(rows, nil)

	return folder.View(store.RunRow{ID: "R", Status: "failed"})
}

// Only the path to a failure or to live work opens by default.
func TestOpenByDefault(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		view Transcript
		want map[string]bool
	}{
		{
			name: "a passed agent with a conversation",
			view: foldEvents(started(1, 0, "agent", "review"), turn(1), finished(1, 0, "agent", "review", "succeeded")),
			want: map[string]bool{"review": false},
		},
		{
			name: "a running agent with a conversation",
			view: foldEvents(started(1, 0, "agent", "review"), turn(1)),
			want: map[string]bool{"review": true},
		},
		{
			name: "a running agent with nothing said yet",
			view: foldEvents(started(1, 0, "agent", "review")),
			want: map[string]bool{"review": false},
		},
		{
			name: "a passed block",
			view: foldEvents(
				started(1, 0, "in_parallel", "checks"),
				started(2, 1, "task", "lint"), finished(2, 1, "task", "lint", "succeeded"),
				finished(1, 0, "in_parallel", "checks", "succeeded"),
			),
			want: map[string]bool{"checks": false, "lint": false},
		},
		{
			// Between children, too: nothing inside is active then.
			name: "a running block",
			view: foldEvents(
				started(1, 0, "in_parallel", "checks"),
				started(2, 1, "task", "lint"), finished(2, 1, "task", "lint", "succeeded"),
			),
			want: map[string]bool{"checks": true, "lint": false},
		},
		{
			name: "a triggering get running its build",
			view: foldEvents(started(1, 0, "get", "upstream"), started(2, 1, "task", "compile")),
			want: map[string]bool{"upstream": true, "compile": false},
		},
		{
			name: "a failed block and its failed child",
			view: foldEvents(
				started(1, 0, "do", "build"),
				started(2, 1, "task", "compile"), finished(2, 1, "task", "compile", "failed"),
				finished(1, 0, "do", "build", "failed"),
			),
			want: map[string]bool{"build": true, "compile": true},
		},
		{
			// The try tolerated it by design; the rollup and f still find it.
			name: "a passed try holding a failure",
			view: foldEvents(
				started(1, 0, "try", "maybe"),
				started(2, 1, "task", "flaky"), finished(2, 1, "task", "flaky", "failed"),
				finished(1, 0, "try", "maybe", "succeeded"),
			),
			want: map[string]bool{"maybe": false, "flaky": true},
		},
		{
			name: "fail_fast aborts a sibling",
			view: foldEvents(
				started(1, 0, "in_parallel", "reviews"),
				started(2, 1, "task", "lint"), finished(2, 1, "task", "lint", "failed"),
				started(3, 1, "agent", "reviewer"), turn(3), finished(3, 1, "agent", "reviewer", "aborted"),
				finished(1, 0, "in_parallel", "reviews", "failed"),
			),
			want: map[string]bool{"reviews": true, "lint": true, "reviewer": false},
		},
		{
			name: "a user abort",
			view: foldEvents(
				started(1, 0, "do", "build"),
				started(2, 1, "task", "compile"), finished(2, 1, "task", "compile", "aborted"),
				finished(1, 0, "do", "build", "aborted"),
			),
			want: map[string]bool{"build": true, "compile": true},
		},
		{
			// Nothing else failed: the abort IS the failure.
			name: "a flat plan a user aborted",
			view: foldEnded(
				started(1, 0, "task", "fetch"), finished(1, 0, "task", "fetch", "succeeded"),
				started(2, 0, "task", "compile"), finished(2, 0, "task", "compile", "aborted"),
			),
			want: map[string]bool{"fetch": false, "compile": true},
		},
		{
			name: "fail_fast aborts a sibling block",
			view: foldEvents(
				started(1, 0, "in_parallel", "reviews"),
				started(2, 1, "do", "security"),
				started(3, 2, "task", "scan"), finished(3, 2, "task", "scan", "failed"),
				finished(2, 1, "do", "security", "failed"),
				started(4, 1, "do", "style"),
				started(5, 4, "agent", "reviewer"), turn(5), finished(5, 4, "agent", "reviewer", "aborted"),
				finished(4, 1, "do", "style", "aborted"),
				finished(1, 0, "in_parallel", "reviews", "failed"),
			),
			want: map[string]bool{"reviews": true, "security": true, "scan": true, "style": false},
		},
		{
			name: "a dropped finish under a finished block",
			view: foldEvents(
				started(1, 0, "do", "build"),
				started(2, 1, "task", "compile"),
				finished(1, 0, "do", "build", "succeeded"),
			),
			want: map[string]bool{"build": true},
		},
		{
			// The same dropped finish once the run has ended: the child is
			// unreported now, not running, and must not be folded away.
			name: "a dropped finish under a finished block on an ended run",
			view: foldEnded(
				started(1, 0, "do", "build"),
				started(2, 1, "task", "compile"),
				finished(1, 0, "do", "build", "succeeded"),
			),
			want: map[string]bool{"build": true, "compile": true},
		},
		{
			name: "an unreported step",
			view: foldEnded(started(1, 0, "task", "compile")),
			want: map[string]bool{"compile": true},
		},
		{
			name: "a passed put",
			view: foldEvents(started(1, 0, "put", "image"), finished(1, 0, "put", "image", "succeeded")),
			want: map[string]bool{"image": true},
		},
		{
			// It would fold shut under the reader the moment the hook passed.
			name: "a task with a running hook",
			view: foldEvents(
				started(1, 0, "task", "build"), finished(1, 0, "task", "build", "succeeded"),
				started(2, 1, "hook", "ensure"),
			),
			want: map[string]bool{"build": false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			seen := 0

			for _, step := range tc.view.Steps {
				want, ok := tc.want[step.Name]
				if !ok {
					continue
				}

				seen++

				if got := step.OpenByDefault(); got != want {
					t.Errorf("%s (%s) OpenByDefault = %v, want %v", step.Name, step.Status, got, want)
				}
			}

			if seen != len(tc.want) {
				t.Fatalf("found %d of the %d steps asked about", seen, len(tc.want))
			}
		})
	}
}
