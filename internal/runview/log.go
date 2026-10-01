package runview

import (
	"context"
	"log/slog"

	"github.com/jtarchie/steps/internal/events"
)

// Log renders a run as log lines, for a process whose runs are read in the browser rather than on its terminal: what Plain prints becomes debug detail, and nothing else is said — the job's start and end are logged where the drain runs it, and every byte a step wrote is in the run's record.
//
// A nil logger is whichever slog.Default() is current when each line is written, rather than the one current when the observer was made.
func Log(ctx context.Context, logger *slog.Logger) func(events.Event) {
	return func(event events.Event) {
		logger := logger
		if logger == nil {
			logger = slog.Default()
		}

		run := []any{"run", event.RunID, "job", event.Job}

		switch event.Type {
		case events.TypeStepStarted:
			logger.DebugContext(ctx, "step.started", append(run, "kind", event.StepKind, "step", event.StepName)...)
		case events.TypeStepSkipped:
			logger.DebugContext(ctx, "step.skipped", append(run, "step", event.StepName, "reason", event.Text)...)
		case events.TypeStepNote:
			logger.DebugContext(ctx, "step.note", append(run, "status", event.Status, "text", event.Text)...)
		}
	}
}
