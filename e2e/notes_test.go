package e2e

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestANoteReachesTheTerminalAndTheTranscript crosses the whole seam a step_note travels: the engine says something about a step, the terminal prints it, and the run's own record keeps it — so the web transcript shows what used to reach only the shell that ran the job. Not parallel: captureStdout swaps os.Stdout.
func TestANoteReachesTheTerminalAndTheTranscript(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: listing
  config:
    check: "echo '[{\"ref\":\"abc\"}]'"
    in: "true"
resources:
- name: repo
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: repo
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

	// Which get a fetched version belongs to: a job with several gets is otherwise a list of versions with no names.
	if fetched := `get: repo (version: {"ref":"abc"})` + "\n"; !strings.Contains(out, fetched) {
		t.Errorf("the terminal did not name the get it fetched, want %q:\n%s", fetched, out)
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

// TestAFetchedVersionIsSaidOnItsGetsRow: the version a get fetched is about that get, so the transcript draws it on the get's row. Recorded against no step, it was drawn in a panel above the whole transcript, apart from the step it describes — for a version: every fan-out, one panel per build with nothing saying which build was which.
func TestAFetchedVersionIsSaidOnItsGetsRow(t *testing.T) {
	for _, mode := range []string{"", "\n    version: every"} {
		path := writePipeline(t, t.TempDir(), `
resource_types:
- name: listing
  config:
    check: "echo '[{\"ref\":\"abc\"}]'"
    in: "true"
resources:
- name: repo
  type: listing
  source: {}
- name: tools
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: repo`+mode+`
  - get: tools
  - task: after
    run: "true"
`)

		mustRun(t, "run", path, "--job", "build")

		rows := latestRunEvents(t, path)

		// The first get fans the plan out and the second fetches inside that build: two paths, each has to name its own row.
		gets := map[string]int64{}
		noted := map[string]int64{}

		for _, row := range rows {
			if row.Type == events.TypeStepStarted && row.StepKind == "get" {
				gets[row.StepName] = row.StepID
			}

			if name, _, ok := strings.Cut(strings.TrimPrefix(row.Text, "get: "), " (version:"); row.Type == events.TypeStepNote && ok {
				noted[name] = row.StepID
			}
		}

		for _, name := range []string{"repo", "tools"} {
			if noted[name] == 0 || noted[name] != gets[name] {
				t.Errorf("get %s%s: its version note is recorded against step %d, want its own row %d", name, mode, noted[name], gets[name])
			}
		}
	}
}

func latestRunEvents(t *testing.T, path string) []store.RunEventRow {
	t.Helper()

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 500)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	return rows
}

// TestHowToResumeIsSaidToTheTerminalOnly: "resume with: steps run …" and "workspace kept at …" are directions for the shell that ran the job; the web page has Retry for the first and no filesystem for the second, and drew both as panels above the transcript.
func TestHowToResumeIsSaidToTheTerminalOnly(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: fails
    run: "exit 1"
`)

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the run to fail")
		}
	})

	for _, said := range []string{"resume with: steps run", "workspace kept at"} {
		if !strings.Contains(out, said) {
			t.Errorf("the terminal was not told %q:\n%s", said, out)
		}

		for _, row := range latestRunEvents(t, path) {
			if strings.Contains(row.Text, said) {
				t.Errorf("the run's record, which the web page draws, holds %q", said)
			}
		}
	}
}
