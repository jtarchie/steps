package e2e

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// TestANoteReachesTheTerminalAndTheTranscript crosses the whole seam a step_note travels: the engine says something about a step, the terminal prints it, and the run's own record keeps it — so the web transcript shows what used to reach only the shell that ran the job. Not parallel: captureStdout swaps os.Stdout.
func TestANoteReachesTheTerminalAndTheTranscript(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - try:
      task: flaky
      run: "exit 3"
  - task: after
    run: "true"
`)

	const said = "try: flaky failed (tried, continuing)"

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "build") })

	if !strings.Contains(out, said+"\n") {
		t.Errorf("the terminal did not print the note %q:\n%s", said, out)
	}

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 500)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	for _, row := range rows {
		if row.Type == events.TypeStepNote && row.Text == said {
			if row.StepID == 0 {
				t.Error("the note was recorded against no step, so the transcript cannot place it")
			}

			return
		}
	}

	t.Errorf("run_events holds no step_note %q", said)
}
