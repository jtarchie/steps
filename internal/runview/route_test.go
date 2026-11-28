package runview

import (
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

// TestPassesNumberOnlyTheStepsARouteSentBack: the second editor and gate are pass 2, the first are unnumbered, and an across:'s cells — which share their block's index and name — are never passes.
func TestPassesNumberOnlyTheStepsARouteSentBack(t *testing.T) {
	t.Parallel()

	view := loop()

	for i, want := range []int{0, 0, 0, 2, 2} {
		if got := view.Steps[i].Pass; got != want {
			t.Errorf("%s (#%d) Pass = %d, want %d", view.Steps[i].Name, view.Steps[i].ID, got, want)
		}
	}

	cells := foldEvents(
		started(1, 0, "across", "matrix"),
		started(2, 1, "task", "cell"), finished(2, 1, "task", "cell", "succeeded"),
		started(3, 1, "task", "cell"), finished(3, 1, "task", "cell", "succeeded"),
		finished(1, 0, "across", "matrix", "succeeded"),
	)

	if pass := cells.Steps[2].Pass; pass != 0 {
		t.Errorf("an across: cell reads as pass %d", pass)
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
