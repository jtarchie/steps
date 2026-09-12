package web

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// panicsOnStep panics recording a finished step, which only happens once the run row exists — past where panicsOnce fires.
type panicsOnStep struct{ store.Store }

func (panicsOnStep) RecordRunStep(context.Context, string, int, string) error {
	panic("step bookkeeping exploded")
}

// A panic after the run started left its row saying running forever: the queue row said failed, the run page said nothing had ended.
func TestAPanicAfterTheRunStartedFinishesTheRun(t *testing.T) {
	t.Parallel()

	runner, target, st := drainable(t, t.TempDir(), `
jobs:
  - name: build
    plan:
      - task: work
        inputs: []
        run: "true"
`)

	target.Store = panicsOnStep{Store: st}

	if !runner.drainOne(t.Context(), target) {
		t.Fatal("nothing was claimed from a queue with a pending row")
	}

	runs, err := st.ListRuns(t.Context(), "build", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].Status != "failed" || runs[0].FinishedAt.IsZero() {
		t.Fatalf("runs = %+v, want the run the panic interrupted finished failed", runs)
	}

	if got := queueStatuses(t, st); got != "failed" {
		t.Errorf("queue = %q, want the panicking run failed", got)
	}
}
