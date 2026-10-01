package runview

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// TestLogIsDebugDetail pins the daemon's terminal: a step's lifecycle and notes are debug lines naming their run, job and step, and nothing else is logged, because the job's own start and end already are and every byte is in the record.
func TestLogIsDebugDetail(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropTime}))
	logged := Log(context.Background(), logger)

	for _, event := range []events.Event{
		{Type: events.TypeJobStarted, RunID: "R", Job: "build"},
		{Type: events.TypeStepStarted, RunID: "R", Job: "build", StepKind: "task", StepName: "compile"},
		{Type: events.TypeStepOutput, RunID: "R", Job: "build", StepName: "compile", Text: "secret-bytes"},
		{Type: events.TypeStepSkipped, RunID: "R", Job: "build", StepName: "test", Text: "when: guard was false"},
		{Type: events.TypeStepNote, RunID: "R", Job: "build", Status: events.NoteWarn, Text: "could not refresh repo"},
		{Type: events.TypeJobFinished, RunID: "R", Job: "build", Status: "failed"},
	} {
		logged(event)
	}

	want := strings.Join([]string{
		`level=DEBUG msg=step.started run=R job=build kind=task step=compile`,
		`level=DEBUG msg=step.skipped run=R job=build step=test reason="when: guard was false"`,
		`level=DEBUG msg=step.note run=R job=build status=warn text="could not refresh repo"`,
	}, "\n") + "\n"

	if out.String() != want {
		t.Errorf("logged:\n%s\nwant:\n%s", out.String(), want)
	}
}

func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}

	return a
}
