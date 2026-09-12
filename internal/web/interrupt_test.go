package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// An interrupted run was never a failure verdict, so a task's fix: agent must not be asked to repair it. Not parallel: t.Setenv.
func TestAnInterruptedFixTaskNeverCallsItsAgent(t *testing.T) {
	var agentHits atomic.Int64

	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		agentHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(endpoint.Close)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	runner, target, st := drainable(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true
agents:
  - name: fixer
    source:
      endpoint: %s/v1/
      model: test-model
      api_key_env: STEPS_TEST_AGENT_API_KEY
jobs:
  - name: build
    interruptible: true
    plan:
      - task: work
        inputs: []
        run: touch %s; sleep 30
        fix: fixer
`, endpoint.URL, started))

	drain, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := drainInBackground(drain, runner, target)

	waitForFile(t, started)
	cancel()
	waitForDrain(t, done, 20*time.Second)

	if got := queueStatuses(t, st); got != "running" {
		t.Errorf("queue = %q, want running: an interrupted build is the next start's to re-queue", got)
	}

	if hits := agentHits.Load(); hits != 0 {
		t.Errorf("the fix agent was called %d times for an interrupted run", hits)
	}
}

// A non-interruptible run's grace is armed by a shutdown, not at start: armed at start it was a ceiling on every build.
func TestARunHasNoDeadlineUntilShutdown(t *testing.T) {
	t.Parallel()

	runner := NewLocalRunner(nil, nil, 1, false)
	runner.StopWith(t.Context())

	runCtx, _, end := runner.runContext(t.Context(), "demo", &config.Job{Name: "build"})
	defer end()

	if deadline, has := runCtx.Deadline(); has {
		t.Errorf("a healthy daemon gave the run a deadline of %s", deadline)
	}
}
