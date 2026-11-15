package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A step's name is pipeline-authored YAML and the listing prints it onto an operator's terminal, so a name holding an escape prints Go-quoted: no raw ESC reaches the terminal, and the line still names the step. Not parallel, it captures stdout.
func TestRunsOutputWithoutAStepQuotesAnOSC(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "\x1b]0;pwned\x07ty")

	var err error

	out := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "-p", "app", "--db", state}) })
	if err != nil || out != `"\x1b]0;pwned\aty"`+"\n" {
		t.Errorf("runs output with no step = %v, printing %q; want the raw name Go-quoted and alone", err, out)
	}
}

// The listing's promise is that a line passes back as <step>; a scrubbed name would not match the stored one, so every listed line, quoted or not, must fetch its step's output. Not parallel, it captures stdout.
func TestRunsOutputListedNamesPassBack(t *testing.T) {
	for _, name := range []string{"plain", "\x1b]0;pwned\x07ty", "two\nlines", "\x1b[0m"} {
		state := filepath.Join(t.TempDir(), "steps.db")
		recordStepOutput(t, state, name)

		var err error

		listed := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "-p", "app", "--db", state}) })
		if err != nil {
			t.Fatalf("listing %q: %v", name, err)
		}

		line := strings.TrimSuffix(listed, "\n")
		if line == "" || strings.ContainsAny(line, "\x1b\n") {
			t.Errorf("step %q listed as %q; want one non-empty, escape-free line", name, line)
		}

		out := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", line, "-p", "app", "--db", state}) })
		if err != nil || out != "hi\n" {
			t.Errorf("runs output RUN %s = %v, printing %q; want the step's output", line, err, out)
		}
	}
}

// Two raw names that scrub alike must not list alike, or one of them could never be asked for. Not parallel, it captures stdout.
func TestRunsOutputNamesThatScrubAlikeListApart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "a\x07", "a")

	var err error

	out := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "-p", "app", "--db", state}) })
	if err != nil || out != "\"a\\a\"\na\n" {
		t.Errorf("runs output with no step = %v, printing %q; want the BEL-bearing name quoted and the plain one bare", err, out)
	}
}

// A step name is unvalidated YAML and may hold a newline; the listing promises one name per line, so a shell loop must not read it as two steps. Not parallel, it captures stdout.
func TestRunsOutputWithoutAStepKeepsAMultilineNameOnOneLine(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "two\nlines")

	var err error

	out := captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "-p", "app", "--db", state}) })
	if err != nil || out != "\"two\\nlines\"\n" {
		t.Errorf("runs output with no step = %v, printing %q; want the name quoted on one line", err, out)
	}
}

// The refusal names the steps that did record output, and an error goes to the same terminal the listing does. Not parallel, it captures stdout.
func TestRunsOutputRefusalScrubsTheNamesItOffers(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "\x1b]0;pwned\x07ty")

	var err error

	_ = captureStdout(t, func() { err = Run([]string{"runs", "output", "RUN", "nobody", "-p", "app", "--db", state}) })
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `these did: "\x1b]0;pwned\aty"`) {
		t.Errorf("runs output nobody = %q, want the offered name Go-quoted", err)
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

// faultyRunsStore is a real store whose run lookup faults, the way a replaced or locked state file does — worded as both drivers word it, so a second wrap shows.
type faultyRunsStore struct {
	store.Store
}

func (faultyRunsStore) FindRunRow(_ context.Context, id string) (store.RunRow, bool, error) {
	return store.RunRow{}, false, fmt.Errorf("could not read run %q: %w", id, errors.New("database disk image is malformed"))
}

// A store fault is reported as a fault, not dressed as an unknown id: a locked database is not a mistyped run. Every command that looks a run up by id says so once.
func TestRunCommandsReportAStoreFailureLookingUpTheRun(t *testing.T) {
	state := filepath.Join(t.TempDir(), "steps.db")
	recordStepOutput(t, state, "chatty")

	st, err := sqlite.OpenExisting(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	faulty := faultyRunsStore{st}

	for name, lookup := range map[string]func() error{
		"runs output": func() error { return (&RunsOutputCmd{RunID: "RUN"}).print(t.Context(), os.Stdout, faulty) },
		"runs steps":  func() error { return (&RunsStepsCmd{RunID: "RUN"}).printRunSteps(t.Context(), os.Stdout, faulty) },
		"runs follow": func() error {
			_, err := (&RunsFollowCmd{RunID: "RUN"}).target(t.Context(), faulty)

			return err
		},
	} {
		err := lookup()
		if err == nil || !strings.Contains(err.Error(), `could not read run "RUN"`) || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("%s: err = %v, want the store's failure named with the run", name, err)
		}

		if err != nil && strings.Count(err.Error(), "could not read run") != 1 {
			t.Errorf("%s: err = %v, want the run named once, not wrapped again over the store's own wording", name, err)
		}

		if err != nil && strings.Contains(err.Error(), "no run") {
			t.Errorf("%s: err = %v, want the fault, not an unknown id", name, err)
		}
	}
}

// recordStepOutput lands one run with one step_output event in a fresh state file.
func recordStepOutput(t *testing.T, state string, stepNames ...string) {
	t.Helper()

	st, err := sqlite.OpenStore(state, "app")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	err = st.StartRun(t.Context(), store.NewRun{ID: "RUN", JobName: "build", Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for _, stepName := range stepNames {
		err = st.AppendRunEvent(t.Context(), store.RunEventRow{RunID: "RUN", Type: events.TypeStepOutput, StepName: stepName, Text: "hi"})
		if err != nil {
			t.Fatalf("AppendRunEvent: %v", err)
		}
	}

	_ = st.Close()
}
