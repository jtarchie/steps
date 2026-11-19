package pipeline

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
)

// events is a leaf and cannot import outcome, so its finish-status spellings are held to outcome.Class here, where both are visible.
func TestEventStatusesAreTheOutcomeClasses(t *testing.T) {
	t.Parallel()

	for status, class := range map[string]outcome.Class{
		events.StatusSucceeded: outcome.Succeeded,
		events.StatusFailed:    outcome.Failed,
		events.StatusErrored:   outcome.Errored,
		events.StatusAborted:   outcome.Aborted,
	} {
		if status != string(class) {
			t.Errorf("events status %q is not outcome class %q", status, class)
		}
	}
}

// recordStepFailureWith and recordBlockAs store store.NodeStatus(class), and store cannot import outcome, so the two spellings are held together here; NodeRunning is never cast from a class.
func TestNodeStatusesAreTheOutcomeClasses(t *testing.T) {
	t.Parallel()

	for class, status := range map[outcome.Class]store.NodeStatus{
		outcome.Succeeded: store.NodeSucceeded,
		outcome.Failed:    store.NodeFailed,
		outcome.Errored:   store.NodeErrored,
		outcome.Aborted:   store.NodeAborted,
	} {
		if store.NodeStatus(class) != status {
			t.Errorf("outcome class %q casts to node status %q, not the stored %q", class, store.NodeStatus(class), status)
		}
	}
}

func TestAJobThatSucceededPublishesTheSucceededStatus(t *testing.T) {
	t.Parallel()

	status, text := runOutcome(t.Context(), nil)
	if status != events.StatusSucceeded || text != "" {
		t.Errorf("runOutcome(nil) = %q, %q; want %q and no text", status, text, events.StatusSucceeded)
	}
}
