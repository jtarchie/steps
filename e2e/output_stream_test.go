package e2e

// A step's output is visible while it runs, and the record kept afterwards ends where the step did (steps#202).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// recordedEvents reads the newest run's events out of the pipeline's state file, or nil while the file or the run does not exist yet.
func recordedEvents(t *testing.T, path string) []store.RunEventRow {
	t.Helper()

	st, err := sqlite.OpenExisting(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(context.Background(), "", 1)
	if err != nil || len(runs) == 0 {
		return nil
	}

	rows, err := st.RunEvents(context.Background(), runs[0].ID, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	return rows
}

// chunkSeenWhileRunning polls the store until a chunk holding text is recorded for a step that has not finished, or the deadline passes.
func chunkSeenWhileRunning(t *testing.T, path, text string, deadline time.Time) bool {
	t.Helper()

	for time.Now().Before(deadline) {
		rows := recordedEvents(t, path)

		for _, row := range rows {
			if row.Type == events.TypeStepOutputChunk && strings.Contains(row.Text, text) && !finishedStep(rows, row.StepID) {
				return true
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	return false
}

// TestARunningStepsOutputIsRecordedAsItPrints: the first line is in the store before the step finishes, so a browser or `steps runs follow` reading the store can show it; once the step ends the live pieces are gone and one output record stands in their place.
func TestARunningStepsOutputIsRecordedAsItPrints(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: slow
    run: "echo first; sleep 3; echo second"
`)

	done := make(chan error, 1)

	go func() { done <- runSilently(path) }()

	// The sleep is the margin: three seconds for a poll to catch the first line recorded while the step is still running, on a loaded machine.
	if !chunkSeenWhileRunning(t, path, "first", time.Now().Add(20*time.Second)) {
		t.Error("the step's first line was never recorded while the step was still running")
	}

	err := <-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	rows := recordedEvents(t, path)

	var outputs []string

	for _, row := range rows {
		switch row.Type {
		case events.TypeStepOutputChunk:
			t.Errorf("a live piece outlived its step: %q", row.Text)
		case events.TypeStepOutput:
			outputs = append(outputs, row.Text)
		}
	}

	if len(outputs) != 1 || outputs[0] != "first\nsecond" {
		t.Errorf("recorded output = %q, want the two lines as one record", outputs)
	}
}

// runSilently runs the build with stdout on /dev/null: these runs print more than a pipe holds, and captureStdout reads its pipe only once the run is over.
func runSilently(path string) error {
	orig := os.Stdout
	defer func() { os.Stdout = orig }()

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()

	os.Stdout = devnull

	err = cli.Run([]string{"run", path, "--job", "build", "--progress", "plain"})
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

func finishedStep(rows []store.RunEventRow, stepID int64) bool {
	for _, row := range rows {
		if row.Type == events.TypeStepFinished && row.StepID == stepID {
			return true
		}
	}

	return false
}

// TestARecordedOutputKeepsItsTail: a step that prints more than the record holds keeps how it started AND how it ended, with the middle marked as elided — the last line is where a six-hour build says why it failed.
func TestARecordedOutputKeepsItsTail(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: chatty
    run: "seq 1 20000"
`)

	_ = runSilently(path)

	rows := recordedEvents(t, path)

	var text string

	for _, row := range rows {
		if row.Type == events.TypeStepOutput {
			text = row.Text
		}
	}

	switch {
	case text == "":
		t.Fatal("no output was recorded")
	case !strings.HasPrefix(text, "1\n2\n3\n"):
		t.Errorf("the record does not start where the step did:\n%.80s", text)
	case !strings.HasSuffix(text, "\n19999\n20000"):
		t.Errorf("the record does not end where the step did:\n%s", text[len(text)-80:])
	case !strings.Contains(text, "elided"):
		t.Errorf("the record does not say that its middle is missing")
	case len(text) > 40_000:
		t.Errorf("the record is %d bytes; the bound is the point", len(text))
	}
}
