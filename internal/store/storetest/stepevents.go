package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

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
