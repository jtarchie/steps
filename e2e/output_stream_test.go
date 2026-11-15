package e2e

// A step's output is visible while it runs, and the record kept afterwards ends where the step did (steps#202).

import (
	"context"
	"fmt"
	"io"
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
	t.Parallel()

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

	assertOneOutputRecord(t, path, "first\nsecond")
}

// TestAColourCodeIsNotCutAcrossLiveChunks (steps#265): a step that pauses right after an ESC has its partial line published by the hold, and the chunk recorded while it runs carries the whole `ESC [31m`, never its tail as text.
func TestAColourCodeIsNotCutAcrossLiveChunks(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: colour
    run: "printf start; sleep 2; printf 'progress \\033'; sleep 2; printf '[31mred\\033[0m\\n'; sleep 3"
`)

	done := make(chan error, 1)

	go func() { done <- runSilently(path) }()

	if !chunkSeenWhileRunning(t, path, "red", time.Now().Add(30*time.Second)) {
		t.Error("the coloured line was never recorded while the step was still running")
	}

	for _, row := range recordedEvents(t, path) {
		if row.Type == events.TypeStepOutputChunk && (strings.HasPrefix(row.Text, "[") || strings.HasSuffix(row.Text, "\x1b")) {
			t.Errorf("a live chunk was cut inside the colour code: %q", row.Text)
		}
	}

	err := <-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestACheckedTasksOutputIsRecordedAsItPrints: a task whose output is judged (assert:, fix:) streams like any other, rather than staying blank until it ends (steps#207).
func TestACheckedTasksOutputIsRecordedAsItPrints(t *testing.T) {
	t.Parallel()

	const run = "echo first; sleep 3; echo second"

	pipelines := map[string]func(dir string) string{
		"assert": func(dir string) string {
			return writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: slow
    run: "`+run+`"
    assert:
      stdout: second
`)
		},
		"fix": func(dir string) string {
			return writeFixPipeline(t, dir, fixAgentServer(t).URL, `"`+run+`"`)
		},
	}

	for name, write := range pipelines {
		t.Run(name, func(t *testing.T) {
			path := write(t.TempDir())
			done := make(chan error, 1)

			go func() { done <- runSilently(path) }()

			if !chunkSeenWhileRunning(t, path, "first", time.Now().Add(20*time.Second)) {
				t.Error("the step's first line was never recorded while the step was still running")
			}

			err := <-done
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			assertOneOutputRecord(t, path, "first\nsecond")
		})
	}
}

// assertOneOutputRecord: once the step ends, its live pieces are gone and one output record holding want stands in their place.
func assertOneOutputRecord(t *testing.T, path, want string) {
	t.Helper()

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

	if len(outputs) != 1 || outputs[0] != want {
		t.Errorf("recorded output = %q, want %q as one record", outputs, want)
	}
}

// runSilently runs the build with its stdout discarded: these runs print more than anything here reads.
func runSilently(path string) error {
	err := cli.RunWith(cli.Streams{Out: io.Discard, Err: os.Stderr}, []string{"run", path, "--job", "build", "--progress", "plain"})
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
	t.Parallel()

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

// TestAResourceScriptsStderrIsRecordedAsItPrints: what a get's in: or a put's out: says on stderr is recorded while it runs and kept afterwards, and stdout is not — for out: it is the version the protocol parses, not something to show (steps#207).
func TestAResourceScriptsStderrIsRecordedAsItPrints(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: slow
  config:
    check: echo '[{"ref":"v1"}]'
    in: "echo fetching >&2; echo in-stdout; sleep 3; echo fetched >&2"
    out: "echo pushing >&2; sleep 3; echo pushed >&2; echo '{\"ref\":\"v2\"}'"

resources:
- name: src
  type: slow
  source: {}
- name: dep
  type: slow
  source: {}

jobs:
- name: build
  plan:
  - get: src
  - get: dep
  - put: src
`)

	done := make(chan error, 1)

	go func() { done <- runSilently(path) }()

	for _, first := range []string{"fetching", "pushing"} {
		if !chunkSeenWhileRunning(t, path, first, time.Now().Add(20*time.Second)) {
			t.Errorf("%q was never recorded while its step was still running", first)
		}
	}

	err := <-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	outputs := map[string][]string{}

	for _, row := range recordedEvents(t, path) {
		if row.Type == events.TypeStepOutput {
			step := row.StepKind + " " + row.StepName
			outputs[step] = append(outputs[step], row.Text)
		}
	}

	// dep is the second get, fetched in place rather than as the build's trigger: the other way a get reaches its script.
	for step, want := range map[string]string{"get src": "fetching\nfetched", "get dep": "fetching\nfetched", "put src": "pushing\npushed"} {
		if got := outputs[step]; len(got) != 1 || got[0] != want {
			t.Errorf("%s recorded %q, want %q as one record", step, got, want)
		}
	}
}
