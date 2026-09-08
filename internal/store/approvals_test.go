package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestApprovalsListsWaitingAndDecided covers both halves of the one listing:
// the waiting list a job is parked behind, and the audit trail of what was
// decided. They differ in filter, order and cap, which is the whole reason
// they are one method with one branch rather than two queries that have to
// agree about the columns.
func TestApprovalsListsWaitingAndDecided(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	first, err := store.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	second, err := store.RequestApproval(ctx, "deploy", "and again?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = store.DecideApproval(ctx, first, "approved", "jtarchie", "looks fine")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	pending, err := store.Approvals(ctx, true, 0)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if len(pending) != 1 || pending[0].ID != second {
		t.Fatalf("waiting approvals = %+v, want only the undecided %d", pending, second)
	}

	if pending[0].Status != "pending" {
		t.Errorf("waiting approval reads status %q, want pending", pending[0].Status)
	}

	all, err := store.Approvals(ctx, false, 10)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if len(all) != 2 || all[0].ID != second {
		t.Fatalf("history = %+v, want both approvals newest first", all)
	}

	// The decision is the audit trail; a listing that dropped it would be a
	// record of who asked and of nothing else.
	decided := all[1]
	if decided.Status != "approved" || decided.DecidedBy != "jtarchie" || decided.Reason != "looks fine" {
		t.Errorf("decided approval = %+v, want approved by jtarchie because it looks fine", decided)
	}
}

// TestPendingApprovalsAreNotCapped: a job is parked behind every waiting
// approval, and the nav badge counts them, so the waiting list takes them all.
// Zero means no limit here the way it does everywhere else.
func TestPendingApprovalsAreNotCapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const asked = 4

	for range asked {
		_, err := store.RequestApproval(ctx, "deploy", "ship it?")
		if err != nil {
			t.Fatalf("RequestApproval: %v", err)
		}
	}

	pending, err := store.Approvals(ctx, true, 0)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if len(pending) != asked {
		t.Fatalf("got %d waiting approvals, want all %d", len(pending), asked)
	}

	// Oldest first: the order somebody should work through them in.
	for i := 1; i < len(pending); i++ {
		if pending[i-1].ID > pending[i].ID {
			t.Errorf("waiting approvals are not oldest-first: %d before %d", pending[i-1].ID, pending[i].ID)
		}
	}
}
