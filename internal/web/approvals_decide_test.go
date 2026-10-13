package web

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// lockedDecisions is a store whose every decision fails the way a busy or full
// database does: not because the row moved on, but because nothing was written.
type lockedDecisions struct {
	store.Store
}

func (lockedDecisions) DecideApproval(context.Context, int64, string, string, string) error {
	return errors.New("database is locked")
}

// TestDecideApprovalSettlesOnlyOnAResubmission: a double click finds the row
// already decided and settles on the first decision, but a write that FAILED
// recorded nothing — redirecting there told the person their approval landed
// while the build sat parked waiting for it.
func TestDecideApprovalSettlesOnlyOnAResubmission(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id, err := pipeline.Store.RequestApproval(ctx, "build", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	if code := post(t, server, "/p/demo/approvals/1", map[string]string{"decision": "approve"}); code != http.StatusSeeOther {
		t.Fatalf("first decision = %d, want a redirect", code)
	}

	if code := post(t, server, "/p/demo/approvals/1", map[string]string{"decision": "reject"}); code != http.StatusSeeOther {
		t.Errorf("a resubmitted decision = %d, want a redirect settling on the first", code)
	}

	if decided, _ := pipeline.Store.ApprovalStatus(ctx, id); decided.Status != "approved" {
		t.Errorf("the resubmission overwrote the decision: %q", decided.Status)
	}

	_, failing := testPipeline(t)
	failing.Store = lockedDecisions{failing.Store}

	server, err = New([]*Pipeline{failing}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = failing.Store.RequestApproval(ctx, "build", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	if code := post(t, server, "/p/demo/approvals/1", map[string]string{"decision": "approve"}); code != http.StatusInternalServerError {
		t.Errorf("a decision the store failed to record = %d, want 500", code)
	}
}
