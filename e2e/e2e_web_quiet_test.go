package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// TestWebTerminalIsLogsOnly: a daemon's run is read in the browser, so its terminal carries log lines and nothing else — no step lifecycle, no note, no byte a step printed — while the run's record still holds every one of them.
func TestWebTerminalIsLogsOnly(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: say
    run: echo quiet-stdout-marker; echo quiet-stderr-marker >&2
`)

	stdout := redirectStdout(t)
	logs := captureStderr(t)
	state := filepath.Join(dir, "daemon.db")
	served := startWeb(t, "--db", state)
	served.state = state

	defer served.stopIfRunning(t)

	// Not captured apart: a daemon line printed while the set is in flight would land in that capture and go unseen.
	served.set(t, "quiet", path)
	served.trigger(t, "quiet", "build")
	waitForLogLine(t, logs, "web.job.done", "pipeline", "quiet")
	served.stop(t)

	for line := range strings.SplitSeq(strings.TrimRight(stdout(), "\n"), "\n") {
		// In-process, the set's client shares this stdout; its two lines are the only ones allowed.
		if !strings.Contains(line, "is new to this daemon") && !strings.Contains(line, "is now serving") {
			t.Errorf("the daemon wrote to stdout, want only log lines on stderr: %q", line)
		}
	}

	logged := logs()
	for _, leaked := range []string{"quiet-stdout-marker", "quiet-stderr-marker", "task: say"} {
		if strings.Contains(logged, leaked) {
			t.Errorf("stderr carries %q, which belongs to the run's record:\n%s", leaked, logged)
		}
	}

	for _, msg := range []string{"web.serving", "web.pipeline.set", "trigger.nothing_to_poll"} {
		findLogLineWith(t, logged, msg, func(string) bool { return true })
	}

	recorded := recordedOutput(t, state, "quiet", "build")
	if !strings.Contains(recorded, "quiet-stdout-marker") || !strings.Contains(recorded, "quiet-stderr-marker") {
		t.Errorf("the run's recorded output = %q, want both markers", recorded)
	}
}

// redirectStdout points os.Stdout at a file for the rest of the test, before a daemon reads it, and returns a reader for what landed there.
func redirectStdout(t *testing.T) func() string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "stdout.log")

	file, err := os.Create(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}

	prev := os.Stdout
	os.Stdout = file

	t.Cleanup(func() {
		os.Stdout = prev
		_ = file.Close()
	})

	return func() string {
		body, readErr := os.ReadFile(path) //nolint:gosec // path is under t.TempDir()
		if readErr != nil {
			t.Fatal(readErr)
		}

		return string(body)
	}
}

// recordedOutput is every step_output the newest run of job recorded.
func recordedOutput(t *testing.T, state, name, job string) string {
	t.Helper()

	st := waitForStore(t, state, name)
	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), job, 10)
	if err != nil || len(runs) == 0 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 500)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	var recorded strings.Builder

	for _, row := range rows {
		if row.Type == events.TypeStepOutput {
			recorded.WriteString(row.Text)
		}
	}

	return recorded.String()
}
