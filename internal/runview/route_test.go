package runview

import (
	"fmt"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

func at(row store.RunEventRow, index int) store.RunEventRow {
	row.StepIndex = index

	return row
}

func routeNote(id int64, target string) store.RunEventRow {
	return store.RunEventRow{Type: events.TypeStepNote, StepIndex: -1, StepID: id, Status: events.NoteInfo, Name: events.NoteRoute, Detail: target, Text: "route: → " + target}
}

func loop() Transcript {
	return foldEvents(
		started(1, 0, "do", "loop"),
		at(started(2, 1, "task", "editor"), 0), at(finished(2, 1, "task", "editor", "succeeded"), 0),
		at(started(3, 1, "task", "gate"), 1), at(finished(3, 1, "task", "gate", "failed"), 1),
		routeNote(3, "editor"),
		at(started(4, 1, "task", "editor"), 0), at(finished(4, 1, "task", "editor", "succeeded"), 0),
		at(started(5, 1, "task", "gate"), 1), at(finished(5, 1, "task", "gate", "succeeded"), 1),
		finished(1, 0, "do", "loop", "succeeded"),
	)
}

// TestARoutedFailureIsNotABreakage: a failure a route consumed is counted, opened and found as the branch it was, not as what turned the run red.
func TestARoutedFailureIsNotABreakage(t *testing.T) {
	t.Parallel()

	view := loop()
	gate := view.Steps[2]

	if !gate.Routed() || gate.RoutedTo != "editor" || gate.Broke() {
		t.Fatalf("gate Routed=%v RoutedTo=%q Broke=%v, want routed to editor", gate.Routed(), gate.RoutedTo, gate.Broke())
	}

	if gate.OpenByDefault() {
		t.Error("a routed failure opens by default, as a breakage does")
	}

	// At the top of the plan too, where the walk does not stop at a passed block above it.
	flat := foldEvents(
		at(started(1, 0, "task", "gate"), 1), at(finished(1, 0, "task", "gate", "failed"), 1),
		routeNote(1, "editor"),
	)

	if failure := flat.InnermostFailure(); failure != nil {
		t.Errorf("InnermostFailure = %q, want none: the route consumed the only failure", failure.Name)
	}

	if rollup := view.Roots[0].Rollup(); rollup.Routed != 1 || rollup.Failed != 0 || rollup.Passed != 3 {
		t.Errorf("rollup = %+v, want 3 passed and 1 routed", rollup)
	}
}

// TestASuccessRouteNamesItsTargetWithoutChangingTheOutcome: a verdict or success route says where it went, and a step that passed stays passed.
func TestASuccessRouteNamesItsTargetWithoutChangingTheOutcome(t *testing.T) {
	t.Parallel()

	view := foldEvents(
		started(1, 0, "agent", "reviewer"), finished(1, 0, "agent", "reviewer", "succeeded"),
		routeNote(1, "verify"),
	)

	reviewer := view.Steps[0]
	if reviewer.RoutedTo != "verify" || reviewer.Routed() || reviewer.Failed() {
		t.Errorf("RoutedTo=%q Routed=%v Failed=%v, want a passed step naming verify", reviewer.RoutedTo, reviewer.Routed(), reviewer.Failed())
	}
}

// TestALoopIsGroupedIntoItsPasses: the rows a route sent the plan back through hang under one loop row, a pass each, and the pass a route closed says where it went.
func TestALoopIsGroupedIntoItsPasses(t *testing.T) {
	t.Parallel()

	view := loop()
	GroupLoops(&view)

	block := view.Roots[0]
	if shape := fmt.Sprintf("%d %s %d", len(block.Children), block.Children[0].Kind, len(block.Children[0].Children)); shape != "1 loop 2" {
		t.Fatalf("the do: holds %s (rows, kind, passes), want one loop of two passes", shape)
	}

	passes := block.Children[0].Children

	for k, want := range []string{"[2 3]", "[4 5]"} {
		if got := fmt.Sprint(stepIDs(passes[k].Children)); got != want {
			t.Errorf("pass %d holds %s, want %s", k+1, got, want)
		}
	}

	if !passes[0].Routed() || passes[0].RoutedTo != "editor" || passes[1].Status != "succeeded" {
		t.Errorf("passes ended %q→%q and %q, want routed to editor then succeeded", passes[0].Status, passes[0].RoutedTo, passes[1].Status)
	}

	if block.Children[0].Key() == passes[0].Key() || passes[0].Key() == passes[1].Key() {
		t.Error("the loop and its passes do not have keys of their own")
	}
}

// TestAPassUnderWayIsRunning: the second pass has not reached its gate again, so it is running whatever its last row did.
func TestAPassUnderWayIsRunning(t *testing.T) {
	t.Parallel()

	partial := foldEvents(
		started(1, 0, "do", "loop"),
		at(started(2, 1, "task", "editor"), 0), at(finished(2, 1, "task", "editor", "succeeded"), 0),
		at(started(3, 1, "task", "gate"), 1), at(finished(3, 1, "task", "gate", "failed"), 1),
		routeNote(3, "editor"),
		at(started(4, 1, "task", "editor"), 0), at(finished(4, 1, "task", "editor", "succeeded"), 0),
	)
	GroupLoops(&partial)

	if second := partial.Roots[0].Children[0].Children[1]; !second.Running() {
		t.Errorf("a pass that has not reached its gate again is %q, want running", second.Status)
	}
}

// TestOnlyABackwardRouteMakesALoop: an across:'s cells share their block's index and name, and a route forward skips ahead; neither repeated anything.
func TestOnlyABackwardRouteMakesALoop(t *testing.T) {
	t.Parallel()

	cells := foldEvents(
		started(1, 0, "across", "matrix"),
		started(2, 1, "task", "cell"), finished(2, 1, "task", "cell", "succeeded"),
		started(3, 1, "task", "cell"), finished(3, 1, "task", "cell", "succeeded"),
		finished(1, 0, "across", "matrix", "succeeded"),
	)
	GroupLoops(&cells)

	if len(cells.Roots[0].Children) != 2 {
		t.Errorf("an across: was grouped into %d rows", len(cells.Roots[0].Children))
	}

	forward := foldEvents(
		at(started(1, 0, "task", "flaky"), 0), at(finished(1, 0, "task", "flaky", "failed"), 0),
		routeNote(1, "report"),
		at(started(2, 0, "task", "report"), 1), at(finished(2, 0, "task", "report", "succeeded"), 1),
	)
	GroupLoops(&forward)

	if len(forward.Roots) != 2 {
		t.Errorf("a forward route was grouped into %d rows", len(forward.Roots))
	}
}

// TestAttemptsSplitAStepsRecordAtEachAttempt: each attempt holds what it printed, numbered where the step holds it, and only the last ends as the step did.
func TestAttemptsSplitAStepsRecordAtEachAttempt(t *testing.T) {
	t.Parallel()

	output := func(text string) store.RunEventRow {
		return store.RunEventRow{Type: events.TypeStepOutput, StepID: 1, StepKind: "task", StepName: "build", Text: text}
	}
	attempt := func(detail string) store.RunEventRow {
		return store.RunEventRow{Type: events.TypeStepNote, StepIndex: -1, StepID: 1, Status: events.NoteInfo, Name: events.NoteAttempt, Detail: detail, Text: "task: build (attempt " + detail + ")"}
	}

	view := foldEvents(
		started(1, 0, "task", "build"),
		output("one"), attempt("2/3"), output("two"), attempt("3/3"), output("three"),
		finished(1, 0, "task", "build", "succeeded"),
	)

	attempts := view.Steps[0].Attempts()
	if len(attempts) != 3 {
		t.Fatalf("got %d attempts, want 3", len(attempts))
	}

	for k, want := range []string{"one", "two", "three"} {
		if got, wanted := fmt.Sprint(attempts[k].N, attempts[k].Of, attempts[k].Outputs), fmt.Sprint(k+1, 3, []NumberedOutput{{I: k, Text: want}}); got != wanted {
			t.Errorf("attempt %d = %s, want %s", k+1, got, wanted)
		}
	}

	if attempts[0].Status != "failed" || attempts[2].Status != "succeeded" || !attempts[2].Last {
		t.Errorf("attempt statuses %q, %q, want failed then the step's", attempts[0].Status, attempts[2].Status)
	}

	if len(view.Steps[0].Notes) != 0 {
		t.Errorf("the attempt notes the tabs replace are still kept: %+v", view.Steps[0].Notes)
	}
}

// TestATriedFailureIsTolerated: a try: that ate its step's failure counts as tolerated, not as what broke the run.
func TestATriedFailureIsTolerated(t *testing.T) {
	t.Parallel()

	view := foldEvents(
		started(9, 0, "do", "block"),
		at(started(1, 9, "try", "notify"), 0),
		at(started(2, 1, "task", "notify"), 0), at(finished(2, 1, "task", "notify", "failed"), 0),
		at(finished(1, 9, "try", "notify", "failed"), 0),
		store.RunEventRow{Type: events.TypeStepNote, StepIndex: -1, StepID: 1, Status: events.NoteInfo, Name: events.NoteTolerated, Text: "try: notify failed (tried, continuing)"},
		finished(9, 0, "do", "block", "succeeded"),
	)

	if rollup := view.Roots[0].Rollup(); rollup.Tolerated != 1 || rollup.Failed != 0 {
		t.Errorf("rollup = %+v, want 1 tolerated and no failure", rollup)
	}

	tried := view.Steps[1]
	if !tried.Tolerated() || tried.Broke() || tried.OpenByDefault() {
		t.Errorf("Tolerated=%v Broke=%v Open=%v, want tolerated, unbroken, folded", tried.Tolerated(), tried.Broke(), tried.OpenByDefault())
	}

	// At the top of the plan, where the walk does not stop at a passed block above it.
	flat := foldEvents(
		at(started(1, 0, "try", "notify"), 0),
		at(started(2, 1, "task", "notify"), 0), at(finished(2, 1, "task", "notify", "failed"), 0),
		at(finished(1, 0, "try", "notify", "failed"), 0),
		store.RunEventRow{Type: events.TypeStepNote, StepIndex: -1, StepID: 1, Status: events.NoteInfo, Name: events.NoteTolerated, Text: "try: notify failed (tried, continuing)"},
	)

	if failure := flat.InnermostFailure(); failure != nil {
		t.Errorf("InnermostFailure = %q, want none", failure.Name)
	}
}

// TestAnAbortAfterARoutedFailureIsNotCollateral: an abort beside a failure is collateral only when that failure broke the run; beside a routed one, the abort is what stopped it, and its row opens.
func TestAnAbortAfterARoutedFailureIsNotCollateral(t *testing.T) {
	t.Parallel()

	view := foldEvents(
		at(started(1, 0, "task", "gate"), 1), at(finished(1, 0, "task", "gate", "failed"), 1),
		routeNote(1, "editor"),
		at(started(2, 0, "task", "editor"), 0), at(finished(2, 0, "task", "editor", "aborted"), 0),
	)

	if !view.Steps[1].OpenByDefault() {
		t.Error("the abort after a routed failure is drawn as collateral, folded shut")
	}
}

func stepIDs(steps []*Step) []int64 {
	ids := make([]int64, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.ID)
	}

	return ids
}
