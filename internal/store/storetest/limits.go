package storetest

// Every listing takes a limit, and the limit means the same thing on each:
// zero is no limit, and anything else is honoured. Pinned here because the
// one test that asserted a listing bound went with the method it bounded, and
// nothing else in the tree noticed a LIMIT clause disappear.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

func mustStartRuns(ctx context.Context, t *testing.T, st store.Store, jobName string, ids ...string) {
	t.Helper()

	for _, id := range ids {
		err := st.StartRun(ctx, id, jobName, "/tmp/ws", "")
		if err != nil {
			t.Fatalf("StartRun(%q): %v", id, err)
		}
	}
}

func (s suite) TestListRunsHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "build", "r1", "r2", "r3")

	all, err := st.ListRuns(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListRuns(0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("ListRuns with limit 0 returned %d runs, want all 3 — zero means no limit", len(all))
	}

	two, err := st.ListRuns(ctx, "build", 2)
	if err != nil {
		t.Fatalf("ListRuns(2): %v", err)
	}

	if len(two) != 2 || two[0].ID != "r3" || two[1].ID != "r2" {
		t.Fatalf("ListRuns with limit 2 = %+v, want the newest two, newest first", two)
	}
}

func (s suite) TestListNodesHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, hash := range []string{"n1", "n2", "n3"} {
		mustRecordNode(t, st, "build", hash)
	}

	all, err := st.ListNodes(ctx, "build", 0)
	if err != nil {
		t.Fatalf("ListNodes(0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("ListNodes with limit 0 returned %d nodes, want all 3", len(all))
	}

	two, err := st.ListNodes(ctx, "build", 2)
	if err != nil {
		t.Fatalf("ListNodes(2): %v", err)
	}

	if len(two) != 2 {
		t.Fatalf("ListNodes with limit 2 returned %d nodes", len(two))
	}
}

func (s suite) TestListTriggerQueueHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	// Three jobs: a pending row is one per job, so three of one job is one row.
	for _, job := range []string{"build", "test", "deploy"} {
		mustEnqueueJob(t, st, job, "poll")
	}

	all, err := st.ListTriggerQueue(ctx, 0)
	if err != nil {
		t.Fatalf("ListTriggerQueue(0): %v", err)
	}

	if len(all) != 3 {
		t.Fatalf("ListTriggerQueue with limit 0 returned %d rows, want all 3", len(all))
	}

	two, err := st.ListTriggerQueue(ctx, 2)
	if err != nil {
		t.Fatalf("ListTriggerQueue(2): %v", err)
	}

	if len(two) != 2 {
		t.Fatalf("ListTriggerQueue with limit 2 returned %d rows", len(two))
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

// seedRunEvents starts two runs and appends three events to the first and
// one to the second, all touching one node hash.
func seedRunEvents(ctx context.Context, t *testing.T, st store.Store) {
	t.Helper()

	mustStartRuns(ctx, t, st, "build", "r1", "r2")

	for i, runID := range []string{"r1", "r1", "r1", "r2"} {
		err := st.AppendRunEvent(ctx, store.RunEventRow{
			RunID: runID, Type: "step_started", StepName: fmt.Sprintf("step-%d", i),
			Hash: "shared-hash", At: time.Now(),
		})
		if err != nil {
			t.Fatalf("AppendRunEvent %d: %v", i, err)
		}
	}
}

func mustRunEvents(ctx context.Context, t *testing.T, st store.Store, runID string, afterSeq int64, limit int) []store.RunEventRow {
	t.Helper()

	rows, err := st.RunEvents(ctx, runID, afterSeq, limit)
	if err != nil {
		t.Fatalf("RunEvents(%q, %d, %d): %v", runID, afterSeq, limit, err)
	}

	return rows
}

// TestRunEventsReplayInOrderFromASeq is the Events facet's whole contract:
// what was appended comes back in order, from a sequence number exclusive,
// and a limit bounds the batch.
func (s suite) TestRunEventsReplayInOrderFromASeq(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	seedRunEvents(ctx, t, st)

	all := mustRunEvents(ctx, t, st, "r1", 0, 0)
	if len(all) != 3 || all[0].StepName != "step-0" || all[2].StepName != "step-2" {
		t.Fatalf("RunEvents for r1 = %+v, want its three events in order", all)
	}

	two := mustRunEvents(ctx, t, st, "r1", 0, 2)
	if len(two) != 2 {
		t.Fatalf("RunEvents with limit 2 returned %d events", len(two))
	}

	rest := mustRunEvents(ctx, t, st, "r1", two[1].Seq, 0)
	if len(rest) != 1 || rest[0].StepName != "step-2" {
		t.Fatalf("RunEvents after seq %d = %+v, want only the third event", two[1].Seq, rest)
	}
}

// TestRunsUsingNodeHonoursItsLimit reads the same table the other way round:
// which runs touched a hash.
func (s suite) TestRunsUsingNodeHonoursItsLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	seedRunEvents(ctx, t, st)

	users, err := st.RunsUsingNode(ctx, "shared-hash", 0)
	if err != nil {
		t.Fatalf("RunsUsingNode(0): %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("RunsUsingNode with limit 0 returned %d runs, want both", len(users))
	}

	one, err := st.RunsUsingNode(ctx, "shared-hash", 1)
	if err != nil {
		t.Fatalf("RunsUsingNode(1): %v", err)
	}

	if len(one) != 1 {
		t.Fatalf("RunsUsingNode with limit 1 returned %d runs", len(one))
	}
}

// TestPassedVersionsAreNewestFirstWithinASecond: a green build records its
// versions in one burst, so most of a job's rows share a timestamp. A limit
// over a timestamp order alone would hand back an arbitrary subset of the
// tied rows on every page load; insertion order breaks the tie.
func (s suite) TestPassedVersionsAreNewestFirstWithinASecond(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	const recorded = 30

	for i := range recorded {
		encoded, err := store.EncodeVersion(map[string]any{"n": fmt.Sprintf("v%02d", i)})
		if err != nil {
			t.Fatal(err)
		}

		err = st.RecordPassedVersion(ctx, "build", "repo", encoded, "b1")
		if err != nil {
			t.Fatalf("RecordPassedVersion %d: %v", i, err)
		}
	}

	all, err := st.PassedVersions(ctx, "build", 0)
	if err != nil {
		t.Fatalf("PassedVersions(0): %v", err)
	}

	if len(all) != recorded {
		t.Fatalf("PassedVersions with limit 0 returned %d rows, want all %d", len(all), recorded)
	}

	page, err := st.PassedVersions(ctx, "build", 25)
	if err != nil {
		t.Fatalf("PassedVersions(25): %v", err)
	}

	if len(page) != 25 {
		t.Fatalf("PassedVersions with limit 25 returned %d rows", len(page))
	}

	for i, row := range page {
		want := fmt.Sprintf(`{"n":"v%02d"}`, recorded-1-i)
		if row.Version != want {
			t.Fatalf("PassedVersions[%d] = %s, want %s — the newest recorded comes first, ties or not", i, row.Version, want)
		}
	}
}

// TestReaderRunsFilterToTheNamedPipelines is why the filter belongs to the
// read rather than to the caller. A state file may hold a pipeline this
// process does not serve, and a feed that fetched a limit and then dropped
// those rows would show fewer runs the busier the pipeline it cannot link to.
func (s suite) TestReaderRunsFilterToTheNamedPipelines(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	app := s.open(t, "app")
	unserved := s.open(t, "unserved")

	mustStartRuns(ctx, t, app, "build", "kept")
	mustStartRuns(ctx, t, unserved, "noise", "n1", "n2", "n3")

	runs, err := app.Reader().RecentRuns(ctx, []string{"app"}, 2)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "kept" || runs[0].Pipeline != "app" {
		t.Fatalf("RecentRuns returned %+v, want only app's one run", runs)
	}

	both, err := app.Reader().RecentRuns(ctx, []string{"app", "unserved"}, 0)
	if err != nil {
		t.Fatalf("RecentRuns(both, 0): %v", err)
	}

	if len(both) != 4 {
		t.Fatalf("RecentRuns over both pipelines with limit 0 returned %d rows, want all 4", len(both))
	}

	// Naming nothing asks for nothing, rather than quietly meaning "all" —
	// an empty served list is a configuration to report, not a wildcard.
	empty, err := app.Reader().RecentRuns(ctx, nil, 10)
	if err != nil {
		t.Fatalf("RecentRuns(nil): %v", err)
	}

	if len(empty) != 0 {
		t.Errorf("RecentRuns(nil) returned %d rows, want none", len(empty))
	}
}

// TestAskQuestionRefusesARunFromAnotherPipeline: a run id alone does not
// scope a question. In a shared state file a sibling pipeline's run exists,
// and a row filed against it would be one this handle could never list back.
func (s suite) TestAskQuestionRefusesARunFromAnotherPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mine := s.open(t, "mine")
	theirs := s.open(t, "theirs")

	mustStartRuns(ctx, t, theirs, "build", "their-run")

	_, _, err := mine.AskQuestion(ctx, store.Question{
		RunID: "their-run", JobName: "build", AgentName: "writer", Question: "Which bump?",
	})
	if err == nil {
		t.Fatal("a question was recorded against another pipeline's run")
	}

	if errors.Is(err, store.ErrQuestionNotPending) {
		t.Fatalf("refused as %v, which is the wrong reason", err)
	}

	pending, err := theirs.Questions(ctx, true, 0)
	if err != nil {
		t.Fatalf("Questions: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("the owning pipeline lists %d question(s) it never asked", len(pending))
	}
}
