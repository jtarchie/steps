package pipeline

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/outcome"
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

func TestAJobThatSucceededPublishesTheSucceededStatus(t *testing.T) {
	t.Parallel()

	status, text := runOutcome(t.Context(), nil)
	if status != events.StatusSucceeded || text != "" {
		t.Errorf("runOutcome(nil) = %q, %q; want %q and no text", status, text, events.StatusSucceeded)
	}
}
