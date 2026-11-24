package e2e

// steps#306 phase 2: a daemon restart does not cost a command running in a container. The restart resumes the run (#208 rung 0), and the interrupted step adopts the command, its volumes and its output count instead of running it again.

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

func restartPipeline(t *testing.T, dir string) string {
	t.Helper()

	return writePipeline(t, dir, `
jobs:
- name: build
  interruptible: true
  plan:
  - task: long
    image: `+dockerE2EImage+`
    volatile: true
    outputs: [out]
    run: |
      echo "id=$(hostname)"
      echo before
      sleep 8
      echo after
      echo done > out/result
  - task: use
    volatile: true
    inputs: [out]
    run: cp out/result `+filepath.Join(dir, "result")+`
`)
}

// runContainers lists the containers labelled with runID, running or not, by name.
func runContainers(t *testing.T, runID string, onlyRunning bool) []string {
	t.Helper()

	args := []string{"ps", "-a", "--filter", "label=steps.run=" + runID, "--format", "{{.Names}}"}
	if onlyRunning {
		args = append(args, "--filter", "status=running")
	}

	out, err := exec.CommandContext(context.Background(), "docker", args...).Output() //nolint:gosec // a filter built from a run id
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}

	return strings.Fields(string(out))
}

func waitForCommand(t *testing.T, runID string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for time.Now().Before(deadline) {
		for _, name := range runContainers(t, runID, true) {
			if strings.HasPrefix(name, "steps-cmd-") {
				return
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("the step's command never started")
}

// runOutput is everything the run recorded the step printing, chunks and records alike, across every occurrence.
func runOutput(t *testing.T, state, name, runID string) string {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.RunEvents(t.Context(), runID, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	var text strings.Builder

	for _, row := range rows {
		if row.Type == events.TypeStepOutput || row.Type == events.TypeStepOutputChunk {
			text.WriteString(row.Text + "\n")
		}
	}

	return text.String()
}

// interruptAndRestart triggers the job, stops the daemon mid-command, calls between while it is down, and restarts it until the run ends; it returns the run's id and the queue's final word.
func interruptAndRestart(t *testing.T, path string, between func(runID string)) (string, string, string) {
	t.Helper()

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "build")

	runID := newestRun(t, served.state, name, "build").ID
	waitForCommand(t, runID)

	// Long enough for "before" and its count to reach the store; the command sleeps well past it.
	time.Sleep(2 * time.Second)

	served.stop(t)

	between(runID)

	restarted := startWeb(t, "--db", served.state, "--interval", "1h")
	restarted.state = served.state
	defer restarted.stopIfRunning(t)

	got := waitForQueueStatuses(t, served.state, name, "build:succeeded")

	restarted.stop(t)

	return runID, got, runOutput(t, served.state, name, runID)
}

// The issue's test: the daemon goes down mid-command and comes back; the same container finishes the step, nothing runs it twice, the output is whole, and the step's outputs are the ones the command wrote.
func TestARestartAdoptsTheCommandItInterrupted(t *testing.T) {
	t.Parallel()
	dockerSlot(t)
	requireDockerE2E(t)

	dir := t.TempDir()

	runID, queue, output := interruptAndRestart(t, restartPipeline(t, dir), func(runID string) {
		if running := runContainers(t, runID, true); len(running) == 0 {
			t.Fatal("the shutdown stopped the command; a run left to resume keeps it")
		}
	})
	if queue != "build:succeeded" {
		t.Fatalf("queue = %s, want the interrupted run resumed to success", queue)
	}

	for _, line := range []string{"id=", "before", "after"} {
		if n := strings.Count(output, line); n != 1 {
			t.Errorf("%q recorded %d times, want once: the command ran again or its output repeated:\n%s", line, n, output)
		}
	}

	if got := readTrimmed(t, filepath.Join(dir, "result")); got != "done" {
		t.Errorf("result = %q, want what the adopted command wrote to its output", got)
	}

	// Commands only: a resumed build is kept whole (rung 0 never tears down a tree it did not create), and its holder with it.
	for _, left := range runContainers(t, runID, false) {
		if strings.HasPrefix(left, "steps-cmd-") {
			t.Errorf("command container left behind: %s", left)
		}
	}
}

// A command gone while the daemon was down (removed, a worker rebooted) is #208's rung 0: the step runs again from the start.
func TestARestartRerunsACommandThatIsGone(t *testing.T) {
	t.Parallel()
	dockerSlot(t)
	requireDockerE2E(t)

	dir := t.TempDir()

	_, queue, output := interruptAndRestart(t, restartPipeline(t, dir), func(runID string) {
		for _, name := range runContainers(t, runID, false) {
			if strings.HasPrefix(name, "steps-cmd-") {
				err := exec.CommandContext(context.Background(), "docker", "rm", "-f", name).Run() //nolint:gosec // a name the daemon listed
				if err != nil {
					t.Fatalf("removing the command: %v", err)
				}
			}
		}
	})
	if queue != "build:succeeded" {
		t.Fatalf("queue = %s, want the run resumed to success", queue)
	}

	if n := strings.Count(output, "id="); n != 2 {
		t.Errorf("the command started %d times, want twice: once cut off, once again:\n%s", n, output)
	}

	if got := readTrimmed(t, filepath.Join(dir, "result")); got != "done" {
		t.Errorf("result = %q, want what the rerun wrote", got)
	}
}
