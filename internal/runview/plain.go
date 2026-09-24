package runview

import (
	"io"

	"github.com/jtarchie/steps/internal/events"
)

// Plain renders a run as the terminal has always read it, one line per thing that happened, for a bus observer to call. Output is not among them: it streams as it is written.
func Plain(w io.Writer) func(events.Event) {
	return func(event events.Event) {
		if line := plainLine(event); line != "" {
			_, _ = io.WriteString(w, line+"\n")
		}
	}
}

func plainLine(event events.Event) string {
	switch event.Type {
	case events.TypeStepStarted:
		if event.StepName == "" {
			return event.StepKind
		}

		return event.StepKind + ": " + event.StepName
	case events.TypeStepSkipped:
		if event.Text == "" {
			return "skip: " + event.StepName
		}

		return "skip: " + event.StepName + " (" + event.Text + ")"
	case events.TypeStepNote:
		if event.Status == events.NoteWarn {
			return "warning: " + event.Text
		}

		return event.Text
	}

	return ""
}
