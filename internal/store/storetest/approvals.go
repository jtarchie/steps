package storetest

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestApprovalsListsOnlyWhatIsWaiting: the waiting list is what a job is
// parked behind and what the nav badge counts, so a decided request must drop
// off it.
func (s suite) TestApprovalsListsOnlyWhatIsWaiting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.twoApprovals(t)

	pending, err := st.Approvals(ctx, true, 0)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if len(pending) != 1 {
		t.Fatalf("waiting approvals = %+v, want only the undecided one", pending)
	}

	if pending[0].Status != "pending" || pending[0].Message != "and again?" {
		t.Errorf("waiting approval = %+v, want the pending request", pending[0])
	}
}

// TestApprovalsListsTheDecisionsNewestFirst: the other half of the same
// listing is the audit trail — who approved a deploy, when, and why a
// rejection was a rejection. Those facts must not depend on external chat
// history, so the row carries them and the listing selects them.
func (s suite) TestApprovalsListsTheDecisionsNewestFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.twoApprovals(t)

	all, err := st.Approvals(ctx, false, 10)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	if len(all) != 2 {
		t.Fatalf("history = %+v, want both approvals", all)
	}

	if all[0].Message != "and again?" {
		t.Errorf("history leads with %q, want the newest request", all[0].Message)
	}

	decided := all[1]
	if decided.Status != "approved" || decided.DecidedBy != "jtarchie" || decided.Reason != "looks fine" {
		t.Errorf("decided approval = %+v, want approved by jtarchie because it looks fine", decided)
	}
}

// twoApprovals records one decided request and one still waiting.
func (s suite) twoApprovals(t *testing.T) store.Store {
	t.Helper()

	ctx := context.Background()

	st := s.open(t, "test")
	t.Cleanup(func() { _ = st.Close() })

	first, err := st.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	_, err = st.RequestApproval(ctx, "deploy", "and again?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = st.DecideApproval(ctx, first, "approved", "jtarchie", "looks fine")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	return st
}

// TestPendingApprovalsAreNotCapped: a job is parked behind every waiting
// approval, and the nav badge counts them, so the waiting list takes them all.
// Zero means no limit here the way it does everywhere else.
func (s suite) TestPendingApprovalsAreNotCapped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	defer func() { _ = st.Close() }()

	const asked = 4

	for range asked {
		_, err := st.RequestApproval(ctx, "deploy", "ship it?")
		if err != nil {
			t.Fatalf("RequestApproval: %v", err)
		}
	}

	pending, err := st.Approvals(ctx, true, 0)
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
