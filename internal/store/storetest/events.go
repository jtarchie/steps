package storetest

// run_events: the transcript of a build, replayed by sequence.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

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

// TestDeleteStepEventsTakesOneTypeOfOneStep: a step's output chunks make way for its output record, and nothing else goes with them — not the step's other events, not another step's chunks, not another pipeline's events.
func (s suite) TestDeleteStepEventsTakesOneTypeOfOneStep(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	other := s.open(t, "other")

	mustStartRuns(ctx, t, st, "build", "run-1")
	mustStartRuns(ctx, t, other, "build", "run-theirs")

	rows := []store.RunEventRow{
		{RunID: "run-1", Type: "step_started", StepID: 1},
		{RunID: "run-1", Type: "step_output_chunk", StepID: 1, Text: "a"},
		{RunID: "run-1", Type: "step_output_chunk", StepID: 1, Text: "b"},
		{RunID: "run-1", Type: "step_output_chunk", StepID: 2, Text: "c"},
		{RunID: "run-1", Type: "step_note", StepID: 1, Text: "kept"},
	}
	for i, row := range rows {
		row.At = time.Now()

		err := st.AppendRunEvent(ctx, row)
		if err != nil {
			t.Fatalf("AppendRunEvent %d: %v", i, err)
		}
	}

	err := other.AppendRunEvent(ctx, store.RunEventRow{RunID: "run-theirs", Type: "step_output_chunk", StepID: 1, Text: "theirs", At: time.Now()})
	if err != nil {
		t.Fatalf("AppendRunEvent (other): %v", err)
	}

	err = st.DeleteStepEvents(ctx, "run-1", 1, "step_output_chunk")
	if err != nil {
		t.Fatalf("DeleteStepEvents: %v", err)
	}

	kept := make([]string, 0, 3)
	for _, row := range mustRunEvents(ctx, t, st, "run-1", 0, 0) {
		kept = append(kept, row.Type+":"+row.Text)
	}

	want := []string{"step_started:", "step_output_chunk:c", "step_note:kept"}
	if len(kept) != len(want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}

	for i := range want {
		if kept[i] != want[i] {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i], want[i])
		}
	}

	if theirs := mustRunEvents(ctx, t, other, "run-theirs", 0, 0); len(theirs) != 1 {
		t.Errorf("the other pipeline's run lost %d of its 1 event to a delete scoped to this one", 1-len(theirs))
	}
}
