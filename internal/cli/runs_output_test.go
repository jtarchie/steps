package cli

import (
	"context"
	"errors"
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

// An unknown run id is not a run that printed nothing: the sentence names the pipeline, under both spellings. The id is quoted with %q rather than printed raw — it is an argv string going onto an operator's terminal, and a control byte in it must not act on that terminal; runview.Scrub is for pipeline-authored names, and one quoted token needs quoting. Not parallel, it captures stdout.
func TestRunsOutputWithAnUnknownRunNamesThePipeline(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "chatty")

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"runs", "output", "NOSUCHRUN"}, `no run "NOSUCHRUN" was recorded for pipeline "app"`},
		{[]string{"runs", "output", "NOSUCHRUN", "chatty"}, `no run "NOSUCHRUN" was recorded for pipeline "app"`},
		{[]string{"runs", "output", "\x1b]0;pwned\x07ty"}, `no run "\x1b]0;pwned\aty" was recorded for pipeline "app"`},
	} {
		var err error

		out := captureStdout(t, func() { err = Run(append(tt.args, "-p", "app", "--db", state)) })
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("steps %s = %v, want %q", strings.Join(tt.args, " "), err, tt.want)
		}

		if err != nil && strings.Contains(err.Error(), "recorded no step output") {
			t.Errorf("steps %s = %v, want the unknown run said so, not a run that printed nothing", strings.Join(tt.args, " "), err)
		}

		if err != nil && strings.Contains(err.Error(), "\x1b") {
			t.Errorf("steps %s = %q, want the id's control byte quoted, not written raw to the terminal", strings.Join(tt.args, " "), err.Error())
		}

		if out != "" {
			t.Errorf("steps %s printed %q to stdout, want nothing", strings.Join(tt.args, " "), out)
		}
	}
}

// A run in the same --db belongs to another pipeline: the lookup is scoped, so it reads as no such run rather than confirming the neighbour's history. Not parallel, it captures stdout.
func TestRunsOutputRefusesAnotherPipelinesRun(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "chatty")

	other, err := sqlite.OpenStore(state, "other")
	if err != nil {
		t.Fatalf("open other's handle: %v", err)
	}

	err = other.StartRun(t.Context(), store.NewRun{ID: "OTHER", JobName: "build", Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = other.AppendRunEvent(t.Context(), store.RunEventRow{RunID: "OTHER", Type: events.TypeStepOutput, StepName: "chatty", Text: "hi"})
	if err != nil {
		t.Fatalf("AppendRunEvent: %v", err)
	}

	_ = other.Close()

	var runErr error

	out := captureStdout(t, func() { runErr = Run([]string{"runs", "output", "OTHER", "-p", "app", "--db", state}) })
	if runErr == nil || !strings.Contains(runErr.Error(), `no run "OTHER" was recorded for pipeline "app"`) {
		t.Errorf("another pipeline's run = %v, want it read as no such run", runErr)
	}

	if out != "" {
		t.Errorf("another pipeline's run printed %q to stdout, want nothing", out)
	}
}

// faultyRunsStore is a real store whose run lookup faults, the way a replaced or locked state file does.
type faultyRunsStore struct {
	store.Store
}

func (faultyRunsStore) FindRunRow(context.Context, string) (store.RunRow, bool, error) {
	return store.RunRow{}, false, errors.New("database disk image is malformed")
}

// A store fault is reported as a fault, not dressed as an unknown id: a locked database is not a mistyped run.
func TestRunsOutputReportsAStoreFailureLookingUpTheRun(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "chatty")

	st, err := sqlite.OpenExisting(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	err = (&RunsOutputCmd{RunID: "RUN"}).print(t.Context(), faultyRunsStore{st})
	if err == nil || !strings.Contains(err.Error(), `could not read run "RUN"`) || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("err = %v, want the store's failure named with the run", err)
	}

	if err != nil && strings.Contains(err.Error(), "no run") {
		t.Errorf("err = %v, want the fault, not an unknown id", err)
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
