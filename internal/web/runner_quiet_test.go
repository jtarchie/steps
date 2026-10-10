package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/workspace"
)

// TestADrainWhoseStoreCallsWorkLogsNoStoreFailure: every finalize and lookup the drain makes logs only when it FAILS, so walking each path with a healthy store must log none of them — an error logged for a call that worked trains an operator to skim past the one that did not. Serial: the logger is the process's.
func TestADrainWhoseStoreCallsWorkLogsNoStoreFailure(t *testing.T) {
	logs := captureLogs(t)

	drainABacklogThenAHeldRow(t)
	drainAPanic(t)
	drainAnAbort(t)

	for _, key := range []string{"web.queue_row", "web.job.backlog_unqueued", "web.complete", "web.job.panic_unrecorded"} {
		if strings.Contains(logs.String(), key) {
			t.Errorf("a store call that worked logged %s:\n%s", key, logs.String())
		}
	}
}

// drainABacklogThenAHeldRow builds two versions, the first queueing the second, then skips a row the breaker holds.
func drainABacklogThenAHeldRow(t *testing.T) {
	t.Helper()

	ctx := t.Context()

	runner, target, st := drainable(t, t.TempDir(), `
resource_types:
  - name: ticker
    config:
      check: printf '[{"n":"1"},{"n":"2"}]'
      in: 'true'

resources:
  - name: ticks
    type: ticker
    source: {}

jobs:
  - name: build
    plan:
      - get: ticks
        version: every
      - task: work
        inputs: []
        run: "true"
`)

	for runner.drainOne(ctx, target) {
	}

	if got := queueStatuses(t, st); got != "succeeded succeeded" {
		t.Fatalf("queue = %q, want one build per version", got)
	}

	if _, active := runner.RunCapacity(); active != 0 {
		t.Errorf("%d builds still hold a slot after the drain finished them", active)
	}

	_, _, err := st.RecordJobOutcome(ctx, "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	err = st.EnqueueJob(ctx, "build", "poll")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	runner.drainOne(ctx, target)

	if got := queueStatuses(t, st); !strings.HasSuffix(got, " skipped") {
		t.Fatalf("queue = %q, want the held job's row skipped", got)
	}
}

func drainAPanic(t *testing.T) {
	t.Helper()

	runner, target, _ := drainable(t, t.TempDir(), `
jobs:
  - name: build
    plan:
      - task: work
        inputs: []
        run: "true"
`)

	provider, err := workspace.NewProvider(target.Config().Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	runner.SetProvider("demo", &panicsOnce{Provider: provider})
	runner.drainOne(t.Context(), target)
}

func drainAnAbort(t *testing.T) {
	t.Helper()

	runner, target, st := slowPipeline(t)
	done := make(chan struct{})

	go func() {
		defer close(done)

		runner.drainOne(context.Background(), target)
	}()

	if !runner.Abort(target, runningRunID(t, st)) {
		t.Fatal("Abort could not find the run this runner is executing")
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the aborted run never returned")
	}

	assertStoppedCleanly(t, st)
}
