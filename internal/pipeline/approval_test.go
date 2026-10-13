package pipeline

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A rejection's error is what the job's failure reads as, so it carries the person's reason, or says there was none.
func TestApprovalRejectionNamesTheReason(t *testing.T) {
	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	for reason, want := range map[string]string{"": "rejected by jt: no reason given", "not today": "rejected by jt: not today"} {
		id, err := st.RequestApproval(t.Context(), "publish", "ship it?")
		if err != nil {
			t.Fatal(err)
		}

		err = st.DecideApproval(t.Context(), id, "rejected", "jt", reason)
		if err != nil {
			t.Fatal(err)
		}

		_, err = awaitApproval(t.Context(), st, id, time.Minute)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("reason %q: error %v, want it to contain %q", reason, err, want)
		}
	}
}

// decidedAtTheDeadline is a person approving in the instant between the wait
// reading "pending" and writing "expired" — the expiry then finds the row
// already decided.
type decidedAtTheDeadline struct {
	store.Store
}

func (s decidedAtTheDeadline) DecideApproval(ctx context.Context, id int64, status store.ApprovalStatus, by, reason string) error {
	err := s.Store.DecideApproval(ctx, id, "approved", "jt", "just in time")
	if err != nil {
		return err //nolint:wrapcheck // a double answering what the store answers
	}

	return s.Store.DecideApproval(ctx, id, status, by, reason) //nolint:wrapcheck // a double answering what the store answers
}

// expiryFails is a store that cannot write the expiry at all.
type expiryFails struct {
	store.Store
}

func (expiryFails) DecideApproval(context.Context, int64, store.ApprovalStatus, string, string) error {
	return errors.New("database is locked")
}

// The expiry is a write like any other: losing the race to a person means the
// person's decision stands, and failing outright must say so rather than read
// as a clean expiry.
func TestApprovalExpiryDefersToADecisionAndReportsAFailedWrite(t *testing.T) {
	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	id, err := st.RequestApproval(t.Context(), "publish", "ship it?")
	if err != nil {
		t.Fatal(err)
	}

	approval, err := awaitApproval(t.Context(), decidedAtTheDeadline{st}, id, 0)
	if err != nil || approval.Status != "approved" || approval.DecidedBy != "jt" {
		t.Errorf("a decision that beat the expiry = %+v, %v; want the approval to stand", approval, err)
	}

	id, err = st.RequestApproval(t.Context(), "publish", "ship it?")
	if err != nil {
		t.Fatal(err)
	}

	_, err = awaitApproval(t.Context(), expiryFails{st}, id, 0)
	if err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("an expiry the store failed to record = %v; want the failure named", err)
	}
}

// The node's result records who decided and why, and only what was said: an empty field is left out, and a step with no decision records nothing.
func TestApprovalRecordKeepsOnlyWhatWasDecided(t *testing.T) {
	t.Parallel()

	if got := approvalRecord(store.Approval{}); got != nil {
		t.Errorf("no approval recorded %v, want nil", got)
	}

	for _, c := range []struct {
		approval store.Approval
		want     map[string]any
	}{
		{
			approval: store.Approval{ID: 7, Status: "approved", DecidedBy: "jt", Reason: "looks good"},
			want:     map[string]any{"approval": int64(7), "status": "approved", "decided_by": "jt", "reason": "looks good"},
		},
		{
			approval: store.Approval{ID: 8, Status: "expired"},
			want:     map[string]any{"approval": int64(8), "status": "expired"},
		},
	} {
		if got := approvalRecord(c.approval); !maps.Equal(got, c.want) {
			t.Errorf("approvalRecord(%+v) = %v, want %v", c.approval, got, c.want)
		}
	}
}
