package storetest

// Approvals: the gate a person decides, and the audit of what was decided.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestDecidingTwiceIsNotPending: the second decision on a row is a
// resubmission, and every caller tells it apart from a write that failed by
// the sentinel — so it must carry it, and must not overwrite the first.
func (s suite) TestDecidingTwiceIsNotPending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	requesting := time.Now()

	id, err := st.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	requested := time.Now()

	pending, err := st.ApprovalStatus(ctx, id)
	if err != nil {
		t.Fatalf("ApprovalStatus: %v", err)
	}

	assertStamped(t, "RequestedAt", pending.RequestedAt, requesting, requested)

	if !pending.DecidedAt.IsZero() {
		t.Errorf("a pending approval was decided at %v", pending.DecidedAt)
	}

	err = st.DecideApproval(ctx, id, store.ApprovalApproved, "jtarchie", "")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	decided, err := st.ApprovalStatus(ctx, id)
	if err != nil {
		t.Fatalf("ApprovalStatus: %v", err)
	}

	assertStamped(t, "DecidedAt", decided.DecidedAt, requested, time.Now())

	err = st.DecideApproval(ctx, id, store.ApprovalRejected, "someone-else", "")
	if !errors.Is(err, store.ErrApprovalNotPending) {
		t.Errorf("deciding an already-decided approval = %v, want store.ErrApprovalNotPending", err)
	}

	err = st.DecideApproval(ctx, id+1000, store.ApprovalApproved, "jtarchie", "")
	if !errors.Is(err, store.ErrApprovalNotPending) {
		t.Errorf("deciding an approval nobody requested = %v, want store.ErrApprovalNotPending", err)
	}

	if again, _ := st.ApprovalStatus(ctx, id); again.Status != store.ApprovalApproved || again.DecidedBy != "jtarchie" {
		t.Errorf("the second decision overwrote the first: %+v", again)
	}
}

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

	if pending[0].Status != store.ApprovalPending || pending[0].Message != "and again?" {
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
	if decided.Status != store.ApprovalApproved || decided.DecidedBy != "jtarchie" || decided.Reason != "looks fine" {
		t.Errorf("decided approval = %+v, want approved by jtarchie because it looks fine", decided)
	}
}

// TestApprovalStatusReadsOneDecision: a parked job polls this for its answer, so it must hand back the decision, and a request that does not exist must be an error rather than an empty answer that never resolves.
func (s suite) TestApprovalStatusReadsOneDecision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	id, err := st.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = st.DecideApproval(ctx, id, store.ApprovalRejected, "jtarchie", "not today")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	approval, err := st.ApprovalStatus(ctx, id)
	if err != nil || approval.ID != id || approval.Status != store.ApprovalRejected || approval.DecidedBy != "jtarchie" || approval.Reason != "not today" {
		t.Errorf("ApprovalStatus = %+v, %v; want the rejection", approval, err)
	}

	_, err = st.ApprovalStatus(ctx, id+1000)
	if err == nil {
		t.Error("ApprovalStatus of a request nobody made answered without an error")
	}
}

// twoApprovals records one decided request and one still waiting.
func (s suite) twoApprovals(t *testing.T) store.Store {
	t.Helper()

	ctx := context.Background()

	st := s.open(t, "test")

	first, err := st.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	_, err = st.RequestApproval(ctx, "deploy", "and again?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = st.DecideApproval(ctx, first, store.ApprovalApproved, "jtarchie", "looks fine")
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

	// Above every bound the tree uses (web lists 200, the CLI 20): fewer and
	// a default cap someone adds to the driver passes here.
	const asked = 201

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

func (s suite) TestApprovalsAuditHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for range 3 {
		_, err := st.RequestApproval(ctx, "deploy", "ship it?")
		if err != nil {
			t.Fatalf("RequestApproval: %v", err)
		}
	}

	all, err := st.Approvals(ctx, false, 0)
	if err != nil {
		t.Fatalf("Approvals(false, 0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("Approvals with limit 0 returned %d rows, want all 3", len(all))
	}

	two, err := st.Approvals(ctx, false, 2)
	if err != nil {
		t.Fatalf("Approvals(false, 2): %v", err)
	}

	if len(two) != 2 {
		t.Fatalf("Approvals with limit 2 returned %d rows", len(two))
	}
}

// TestAResumedStepKeepsItsApproval: a step that asks again for the same run, because a restart cut it off while it waited, gets the approval it already made rather than a second one nobody is waiting on.
func (s suite) TestAResumedStepKeepsItsApproval(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "RUN1", "RUN2")

	first, err := st.RequestApprovalFor(ctx, "RUN1", "step-a", "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApprovalFor: %v", err)
	}

	again, err := st.RequestApprovalFor(ctx, "RUN1", "step-a", "deploy", "ship it?")
	if err != nil || again != first {
		t.Fatalf("the same step asking again got approval %d (%v), want the one it made: %d", again, err, first)
	}

	for name, ask := range map[string]func() (int64, error){
		"another step":  func() (int64, error) { return st.RequestApprovalFor(ctx, "RUN1", "step-b", "deploy", "ship it?") },
		"another run":   func() (int64, error) { return st.RequestApprovalFor(ctx, "RUN2", "step-a", "deploy", "ship it?") },
		"no run at all": func() (int64, error) { return st.RequestApprovalFor(ctx, "", "step-a", "deploy", "ship it?") },
	} {
		id, err := ask()
		if err != nil || id == first {
			t.Errorf("%s got approval %d (%v), want one of its own", name, id, err)
		}
	}

	pending, err := st.Approvals(ctx, true, 0)
	if err != nil || len(pending) != 4 {
		t.Fatalf("pending = %d (%v), want four: the one step asked twice, and three others", len(pending), err)
	}
}

// TestAnApprovalNamesItsRun: a run page offers the approvals its run is parked on, found by the run each one names; one asked with no run names none.
func (s suite) TestAnApprovalNamesItsRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "RUN1")

	id, err := st.RequestApprovalFor(ctx, "RUN1", "step-a", "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApprovalFor: %v", err)
	}

	_, err = st.RequestApproval(ctx, "deploy", "no run?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	got, err := st.ApprovalStatus(ctx, id)
	if err != nil || got.RunID != "RUN1" {
		t.Errorf("approval %d reads back run %q (%v), want RUN1", id, got.RunID, err)
	}

	pending, err := st.Approvals(ctx, true, 0)
	if err != nil || len(pending) != 2 || pending[0].RunID != "RUN1" || pending[1].RunID != "" {
		t.Errorf("pending = %+v (%v), want RUN1's then one naming no run", pending, err)
	}
}

// TestAGrantedApprovalStandsForTheResumedStep: somebody may approve while the daemon is down, and the step that resumes must find that approval rather than ask again. A refusal or an expiry is not carried over: a retry asks a person afresh.
func (s suite) TestAGrantedApprovalStandsForTheResumedStep(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	startRuns(t, st, "RUN1")

	granted, err := st.RequestApprovalFor(ctx, "RUN1", "step-a", "deploy", "ship it?")
	if err != nil {
		t.Fatal(err)
	}

	err = st.DecideApproval(ctx, granted, store.ApprovalApproved, "jt", "")
	if err != nil {
		t.Fatal(err)
	}

	resumed, err := st.RequestApprovalFor(ctx, "RUN1", "step-a", "deploy", "ship it?")
	if err != nil || resumed != granted {
		t.Fatalf("a resumed step got approval %d (%v), want the granted one: %d", resumed, err, granted)
	}

	refused, err := st.RequestApprovalFor(ctx, "RUN1", "step-b", "deploy", "ship it?")
	if err != nil {
		t.Fatal(err)
	}

	err = st.DecideApproval(ctx, refused, store.ApprovalRejected, "jt", "no")
	if err != nil {
		t.Fatal(err)
	}

	retried, err := st.RequestApprovalFor(ctx, "RUN1", "step-b", "deploy", "ship it?")
	if err != nil || retried == refused {
		t.Fatalf("a retry after a refusal got approval %d (%v), want a new one", retried, err)
	}
}

// TestARequestSurvivesBeingDecidedAtOnce: a request returns the id of the row it made even when a decision lands the instant that row is visible. A request that looked its row up by "still pending" lost it to a fast decider and reported a failure for work that had succeeded.
func (s suite) TestARequestSurvivesBeingDecidedAtOnce(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	requester := s.open(t, "test")
	decider := s.open(t, "test")

	startRuns(t, requester, "RUN1")

	stop := make(chan struct{})

	var deciders sync.WaitGroup

	for range 8 {
		deciders.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				pending, err := decider.Approvals(ctx, true, 100)
				if err != nil {
					return
				}

				for _, approval := range pending {
					_ = decider.DecideApproval(ctx, approval.ID, store.ApprovalRejected, "test", "no")
				}
			}
		})
	}

	t.Cleanup(deciders.Wait)
	t.Cleanup(func() { close(stop) })

	for i := range 300 {
		_, err := requester.RequestApprovalFor(ctx, "RUN1", "step-"+strconv.Itoa(i), "deploy", "ship it?")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}
