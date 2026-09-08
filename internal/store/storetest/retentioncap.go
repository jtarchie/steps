package storetest

// What retention bounds that a caller of the contract can see.

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestPruneBoundsTheTriggerQueue: the queue is a work list, not a history. A
// finished row means nothing to anything that reads the table, and a FAILED
// one carries the error that stopped the job — for a check, the whole
// generated script, about 1.3KB. A remote that is down at a one-minute poll
// wrote that same 1.3KB every minute, forever.
//
// Pending and running rows are never touched, because those ARE the work list:
// reaping one loses a job nothing will re-queue.
func (s suite) TestPruneBoundsTheTriggerQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	const (
		finished = 6
		keep     = 2
	)

	// One pending row per job at a time, so each is claimed and completed
	// before the next is enqueued — which is how a poller reaches a pile of
	// finished rows in the first place.
	for range finished {
		mustEnqueueJob(t, st, "build", "resource-a")

		id, _ := mustClaimJob(t, st, "build")

		err := st.CompleteJob(ctx, id, "failed", errors.New("the remote is down"))
		if err != nil {
			t.Fatalf("CompleteJob: %v", err)
		}
	}

	// One row left in flight, which retention must leave alone.
	mustEnqueueJob(t, st, "build", "resource-b")

	err := st.Prune(ctx, store.Retention{JobName: "build", TriggerQueue: keep}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	rows, err := st.ListTriggerQueue(ctx, 100)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	var kept, pending int

	for _, row := range rows {
		if row.Status == "pending" {
			pending++

			continue
		}

		kept++
	}

	if kept != keep {
		t.Errorf("%d finished rows survive a cap of %d", kept, keep)
	}

	if pending != 1 {
		t.Errorf("%d pending rows survive, want the one still queued — retention took work off the list", pending)
	}
}
