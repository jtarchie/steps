package pipeline

import (
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
