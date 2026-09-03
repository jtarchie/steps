package e2e

// The flags `steps web` absorbed when it became the only daemon, tested
// across the seam between the flag and the thing it configures.
//
// internal/web proves the drainer runs N jobs at once and that it passes its
// pins to a run; these prove the values the operator typed are the ones it
// gets. That is the half a package-level test cannot see, and the shape of
// bug this repo has been bitten by before: two well-tested units with nothing
// carrying a value across them.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
)

// TestWebMaxConcurrentReachesTheDrainer.
//
// The two jobs rendezvous through the filesystem, so they can only both
// SUCCEED if they run at the same time: one at a time, the first blocks on a
// file the second cannot write until it returns, and the step timeout ends it
// as a failure.
func TestWebMaxConcurrentReachesTheDrainer(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.flag")
	second := filepath.Join(dir, "second.flag")

	path := writePipeline(t, dir, `
jobs:
- name: first
  plan:
  - task: rendezvous
    inputs: []
    timeout: 20s
    run: |
      touch `+first+`
      until [ -f `+second+` ]; do sleep 0.05; done
- name: second
  plan:
  - task: rendezvous
    inputs: []
    timeout: 20s
    run: |
      touch `+second+`
      until [ -f `+first+` ]; do sleep 0.05; done
`)

	st, err := store.OpenStore(cli.StatePath(path, ""), cli.PipelineName(path))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	for _, job := range []string{"first", "second"} {
		err = st.EnqueueJob(t.Context(), job, "test")
		if err != nil {
			t.Fatalf("EnqueueJob %s: %v", job, err)
		}
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	// A long interval: this pipeline has nothing to poll, and the queue rows
	// are already there. What is being tested is the drain.
	served := startWeb(t, []string{path}, "--max-concurrent", "2", "--interval", "1h")
	defer served.stop(t)

	waitForQueueSuccess(t, path, 2)
}

// waitForQueueSuccess waits until `want` queue rows have succeeded, failing
// on the first row that did not.
func waitForQueueSuccess(t *testing.T, pipelinePath string, want int) {
	t.Helper()

	deadline := time.Now().Add(40 * time.Second)

	for time.Now().Before(deadline) {
		st, err := store.OpenStore(cli.StatePath(pipelinePath, ""), cli.PipelineName(pipelinePath))
		if err != nil {
			t.Fatalf("open state store: %v", err)
		}

		rows, err := st.ListTriggerQueue(t.Context(), 10)
		_ = st.Close()

		if err != nil {
			t.Fatalf("ListTriggerQueue: %v", err)
		}

		succeeded := 0

		for _, row := range rows {
			if row.Status == "failed" {
				t.Fatalf("job %s failed (%s): the drainer ran the queue one job at a time", row.JobName, row.Error)
			}

			if row.Status == "succeeded" {
				succeeded++
			}
		}

		if succeeded >= want {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("fewer than %d queued jobs finished: the drainer ran them one at a time", want)
}

// TestWebPinReachesTheRun.
//
// --pin was `steps watch`'s, and the browser-driven runner ignored pins
// entirely — so folding the daemons without threading it would have made the
// flag parse and do nothing. Cold start builds the NEWEST version, so a
// pinned run building anything else is only explicable by the pin.
func TestWebPinReachesTheRun(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 3)

	served := startWeb(t, []string{fixture.pipeline}, "--interval", "200ms", "--pin", "n=2")
	defer served.stop(t)

	waitForDid(t, fixture, "2")
}
