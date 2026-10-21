package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestAPreflightRefusalIsAnErroredRunWithNoSteps: a job preflight refuses used
// to leave no run at all — a failed queue row under `steps web`, a line on
// the terminal, and nothing on the board. The refusal is now a run: errored,
// with no steps, carrying the reason, so the job turns red, `steps runs`
// lists it, and the job's on_error fires (the machinery said no, not a step).
func TestAPreflightRefusalIsAnErroredRunWithNoSteps(t *testing.T) {
	isolatePreflightPins(t)

	dir := t.TempDir()
	errorLog := filepath.Join(dir, "error.log")
	failureLog := filepath.Join(dir, "failure.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: reviewer
  source:
    endpoint: http://127.0.0.1:1/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: outage
  plan:
  - agent: reviewer
    messages:
      - Review it.
  on_error:
    task: page
    run: echo errored >> %s
  on_failure:
    task: wrong
    run: echo failed >> %s
`, errorLog, failureLog))

	err := cli.Run([]string{"run", path, "--job", "outage"})
	if err == nil {
		t.Fatal("run should have been refused: the provider is unreachable")
	}

	if !strings.Contains(err.Error(), "preflight failed, no steps were run") {
		t.Fatalf("error does not say preflight refused it: %v", err)
	}

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "outage", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1: a refused job is still a run", len(runs))
	}

	if runs[0].Status != store.RunFailed {
		t.Errorf("run status = %q, want %q", runs[0].Status, store.RunFailed)
	}

	assertRefusedRunRecord(t, st, runs[0].ID)

	// Infrastructure said no, so on_error — and the hook ran, in the
	// workspace the run now has.
	assertLineCount(t, errorLog, 1)
	assertNoFile(t, failureLog)
}

// assertRefusedRunRecord: the run ended errored carrying the preflight reason,
// and no plan step ever started. The job's hooks are rows of the transcript
// too; only a PLAN step starting would mean something was spent.
func assertRefusedRunRecord(t *testing.T, st store.Store, runID string) {
	t.Helper()

	rows, err := st.RunEvents(t.Context(), runID, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	var finished *store.RunEventRow

	for i := range rows {
		switch rows[i].Type {
		case events.TypeStepStarted:
			if rows[i].StepKind != "hook" {
				t.Errorf("%s %q started on a run preflight refused", rows[i].StepKind, rows[i].StepName)
			}
		case events.TypeJobFinished:
			finished = &rows[i]
		}
	}

	if finished == nil {
		t.Fatal("the run never recorded how it ended")
	}

	if finished.Status != "errored" || !strings.Contains(finished.Text, "preflight failed, no steps were run") {
		t.Errorf("run ended %q with %q, want errored carrying the preflight reason", finished.Status, finished.Text)
	}
}

// TestRetryOfARefusedRunResolvesFreshAndTakes: a refused run recorded no
// input versions, so a rerun had nothing to rebuild against and was refused
// ("recorded no input versions") — on exactly the run a person retries after
// fixing the configuration. It now resolves afresh and takes what it builds,
// as the refused run would have, still linked to the run it retries.
func TestRetryOfARefusedRunResolvesFreshAndTakes(t *testing.T) {
	isolatePreflightPins(t)

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"one"}]`)

	pipeline := func(endpoint string) string {
		return fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

agents:
- name: writer
  source:
    endpoint: %[3]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: note
    inputs: [ticks]
    run: cat ticks/n.txt >> %[2]s
  - agent: writer
    messages:
      - Say done.
`, versions, ran, endpoint)
	}

	path := writePipeline(t, dir, pipeline("http://127.0.0.1:1"))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err == nil || !strings.Contains(err.Error(), "preflight failed") {
		t.Fatalf("the first run should have been refused by preflight, got %v", err)
	}

	assertNoFile(t, ran)

	refused := latestRunID(t, path)

	// The configuration is fixed: the agent now points at a provider that answers.
	live := newFakeLLM(t, says("probe ok"), says("done"), says("done"))
	writePipeline(t, dir, pipeline(live.URL))

	mustRun(t, "run", path, "--rerun", refused)

	if got := strings.TrimSpace(readFileString(t, ran)); got != "one" {
		t.Fatalf("the retry built %q, want %q: it should resolve the version the refused run never reached", got, "one")
	}

	st := openStoreFor(t, path)

	retry, ok, err := st.FindRunRow(t.Context(), latestRunID(t, path))
	if err != nil || !ok {
		t.Fatalf("FindRunRow: %v, %v", ok, err)
	}

	if retry.RerunOf != refused {
		t.Errorf("the retry records rerun of %q, want the refused run %q", retry.RerunOf, refused)
	}

	// The retry TOOK "one": a newer version arriving is the only thing the
	// next ordinary run builds, where a rerun's usual take-nothing would
	// have left "one" to be built a second time.
	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	mustRun(t, "run", path, "--job", "build")

	if got := strings.Fields(readFileString(t, ran)); strings.Join(got, " ") != "one two" {
		t.Errorf("builds read %v, want [one two]: the retry did not take the version it built", got)
	}
}
