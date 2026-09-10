package e2e

// What an abort means is Concourse's — its own status, hooks still run, nothing cached, a queued build never starts — and these pin that it survives the trip from outside the process to the RIGHT run.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// The re-run is the cache and still-serving assertion at once: a cached abort replays and leaves one tally line, a dead drain or a held slot never finishes it.
func TestAbortStopsOneRunAndTheDaemonKeepsServing(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	tally := filepath.Join(dir, "tally")
	hooks := filepath.Join(dir, "hooks")

	path := writePipeline(t, dir, `
jobs:
- name: slow
  plan:
  - task: wait
    inputs: []
    run: |
      echo ran >> `+tally+`
      touch `+started+`
      [ -f `+release+` ] || sleep 60
    on_abort:
      task: noted
      inputs: []
      run: echo abort >> `+hooks+`
    on_failure:
      task: misread
      inputs: []
      run: echo failure >> `+hooks+`
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "slow")
	waitForFile(t, started)

	runID := newestRun(t, served.state, name, "slow").ID

	err := cli.Run([]string{"runs", "abort", runID, "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs abort: %v", err)
	}

	waitForRunStatus(t, served.state, name, runID, "aborted")

	if got := readTrimmed(t, hooks); got != "abort" {
		t.Errorf("hooks fired = %q, want exactly on_abort", got)
	}

	if got := queueStatuses(t, served.state, name); got != "slow:aborted" {
		t.Errorf("queue = %s, want the row finalized aborted — left running, the next restart re-runs a build somebody stopped", got)
	}

	err = os.WriteFile(release, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	served.trigger(t, name, "slow")
	waitForQueueSuccess(t, served.state, name, 1)

	if got := readTrimmed(t, tally); got != "ran\nran" {
		t.Errorf("tally = %q, want the step to run twice: the aborted step was cached as if it passed", got)
	}

	served.stop(t)
}

// Claims go oldest first, so quick succeeding proves the drain walked past the dropped row rather than building slow a second time.
func TestAbortDropsAQueuedRunBeforeItStarts(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	path := writePipeline(t, dir, `
jobs:
- name: slow
  plan:
  - task: wait
    inputs: []
    run: |
      touch `+started+`
      sleep 60
- name: quick
  plan:
  - task: hello
    inputs: []
    run: echo hello
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "slow")
	waitForFile(t, started)

	// The job's one max_in_flight slot is taken, so this waits in the queue.
	served.trigger(t, name, "slow")

	err := cli.Run([]string{"runs", "abort", "--queued", "slow", "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs abort --queued: %v", err)
	}

	running := newestRun(t, served.state, name, "slow")

	err = cli.Run([]string{"runs", "abort", running.ID, "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs abort: %v", err)
	}

	waitForRunStatus(t, served.state, name, running.ID, "aborted")

	served.trigger(t, name, "quick")
	waitForQueueSuccess(t, served.state, name, 1)

	if got := len(listRuns(t, served.state, name, "slow")); got != 1 {
		t.Errorf("slow ran %d times, want 1: the queued build was aborted and must never start", got)
	}

	err = cli.Run([]string{"runs", "abort", "--queued", "slow", "-p", name, "--target", served.target()})
	if err == nil || !strings.Contains(err.Error(), "nothing") {
		t.Errorf("aborting a queue with nothing in it = %v, want a refusal saying so", err)
	}

	served.stop(t)
}

// The seam: a cancel that stopped at the orchestrator leaves the worker's process running — on a real worker, billing — under a run that reads aborted.
func TestAbortReachesAPlacedStepOnAWorker(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	path := writePipeline(t, dir, `
jobs:
- name: placed
  plan:
  - task: wait
    tags: [box]
    inputs: []
    run: |
      echo $$ > `+pidFile+`
      exec sleep 60
`)

	served := startWebFor(t, path, "--interval", "1h", "--worker", "box=local:")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "placed")
	waitForFile(t, pidFile)

	pid, err := strconv.Atoi(readTrimmed(t, pidFile))
	if err != nil {
		t.Fatalf("pid file: %v", err)
	}

	runID := newestRun(t, served.state, name, "placed").ID

	status := served.post(t, fmt.Sprintf("/p/%s/runs/%s/abort", name, runID))
	if status != http.StatusSeeOther {
		t.Fatalf("the abort button answered %d, want 303 back to the run", status)
	}

	waitForRunStatus(t, served.state, name, runID, "aborted")

	deadline := time.Now().Add(10 * time.Second)

	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("the placed step's process (pid %d) outlived the abort: the cancel never crossed to the worker", pid)
		}

		time.Sleep(50 * time.Millisecond)
	}

	served.stop(t)
}

// Redirects unfollowed, so the answer is the route's own rather than the page it sends the browser to.
func (w *webProcess) post(t *testing.T, path string) int {
	t.Helper()

	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+w.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir() file the test's own pipeline wrote
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return strings.TrimSpace(string(data))
}

func listRuns(t *testing.T, state, name, job string) []store.RunRow {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), job, 20)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	return runs
}

// newestRun waits for the job's run row, which a claim creates a moment after the step's first sign of life can be seen.
func newestRun(t *testing.T, state, name, job string) store.RunRow {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		runs := listRuns(t, state, name, job)
		if len(runs) > 0 {
			return runs[0]
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("no run of %s was recorded", job)

	return store.RunRow{}
}

func waitForRunStatus(t *testing.T, state, name, runID, want string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	last := ""

	for time.Now().Before(deadline) {
		for _, run := range listRuns(t, state, name, "") {
			if run.ID == runID {
				last = run.Status
			}
		}

		if last == want {
			return
		}

		if last != "running" && last != "" {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("run %s is %q, want %q", runID, last, want)
}

// queueStatuses is the queue oldest first, as job:status pairs.
func queueStatuses(t *testing.T, state, name string) string {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.ListTriggerQueue(t.Context(), 20)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	pairs := make([]string, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		pairs = append(pairs, rows[i].JobName+":"+rows[i].Status)
	}

	return strings.Join(pairs, " ")
}
