package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A step's name is pipeline-authored YAML and the listing prints it onto an operator's terminal, so it goes through the same scrub as every other untrusted text; not parallel, it captures stdout.
func TestRunsOutputWithoutAStepDropsAnOSC(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "\x1b]0;pwned\x07ty")

	var err error

	out := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "-p", "app", "--db", state}) })
	if err != nil || out != "ty\n" {
		t.Errorf("runs output with no step = %v, printing %q; want the step's name, scrubbed and alone", err, out)
	}
}

// Asking which steps recorded output is a read like the other runs views: it says what it found and creates no database. Not parallel, it captures stdout.
func TestRunsOutputAgainstNoStateStillSaysSo(t *testing.T) {
	db := filepath.Join(t.TempDir(), "none.db")

	var err error

	out := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "-p", "app", "--db", db}) })
	if err != nil || !strings.Contains(out, "no runs recorded yet for app") {
		t.Errorf("runs output with no state = %v, printing %q; want the empty-state sentence", err, out)
	}

	_, statErr := os.Stat(db)
	if !os.IsNotExist(statErr) {
		t.Errorf("runs output created %s by asking about it", db)
	}
}

// recordStepOutput lands one run with one step_output event in a fresh state file.
func recordStepOutput(t *testing.T, state, stepName string) {
	t.Helper()

	st, err := sqlite.OpenStore(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	err = st.StartRun(t.Context(), store.NewRun{ID: "RUN", JobName: "build", Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = st.AppendRunEvent(t.Context(), store.RunEventRow{RunID: "RUN", Type: events.TypeStepOutput, StepName: stepName, Text: "hi"})
	if err != nil {
		t.Fatalf("AppendRunEvent: %v", err)
	}

	_ = st.Close()
}
