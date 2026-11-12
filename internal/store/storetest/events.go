package storetest

// run_events: the transcript of a build, replayed by sequence.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// seedRunEvents starts two runs and appends three events to the first and
// one to the second, all touching one node hash.
func seedRunEvents(ctx context.Context, t *testing.T, st store.Store) {
	t.Helper()

	seedRuns(t, st, "build", "r1", "r2") //nolint:contextcheck // seedRuns takes the test context; the ctx here is that same one

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

	ctx := ctxFor(t)
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

	ctx := ctxFor(t)
	st := s.open(t, "test")
	other := s.open(t, "other")

	seedRuns(t, st, "build", "run-1")
	seedRuns(t, other, "build", "run-theirs")

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

// readOutput is one kept output joined back together.
func readOutput(ctx context.Context, t *testing.T, st store.Store, runID string, stepID int64, n int) (string, bool) {
	t.Helper()

	var whole []byte

	found, err := st.StepOutput(ctx, runID, stepID, n, func(chunk []byte) error {
		whole = append(whole, chunk...)

		return nil
	})
	if err != nil {
		t.Fatalf("StepOutput(%q, %d, %d): %v", runID, stepID, n, err)
	}

	return string(whole), found
}

// seedStepOutputs writes run-1's chunks out of order: seq, not arrival, orders a read.
func seedStepOutputs(ctx context.Context, t *testing.T, st store.Store) {
	t.Helper()

	seedRuns(t, st, "build", "run-1") //nolint:contextcheck // seedRuns takes the test context; the ctx here is that same one

	for _, c := range []struct {
		step   int64
		n, seq int
		text   string
	}{{1, 0, 1, "world\n"}, {1, 0, 0, "hello "}, {1, 1, 0, "second attempt"}, {2, 0, 0, "other step"}} {
		err := st.AppendStepOutput(ctx, "run-1", c.step, c.n, c.seq, []byte(c.text))
		if err != nil {
			t.Fatalf("AppendStepOutput: %v", err)
		}
	}
}

// TestStepOutputComesBackWholeInOrder: chunks are one output read in seq order, each (step, n) apart from the others.
func (s suite) TestStepOutputComesBackWholeInOrder(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	seedStepOutputs(ctx, t, st)

	if got, ok := readOutput(ctx, t, st, "run-1", 1, 0); !ok || got != "hello world\n" {
		t.Errorf("step 1 output 0 = %q, %v; want its two chunks joined in seq order", got, ok)
	}

	if got, _ := readOutput(ctx, t, st, "run-1", 1, 1); got != "second attempt" {
		t.Errorf("step 1 output 1 = %q, want the second attempt's alone", got)
	}

	for _, missing := range []struct {
		run  string
		step int64
		n    int
	}{{"run-1", 3, 0}, {"run-1", 1, 2}, {"run-nobody", 1, 0}} {
		if got, ok := readOutput(ctx, t, st, missing.run, missing.step, missing.n); ok || got != "" {
			t.Errorf("%+v read %q, %v; want nothing found", missing, got, ok)
		}
	}
}

// TestStepOutputIsScopedToThePipeline: a run of another pipeline reads as nothing, and cannot be written to.
func (s suite) TestStepOutputIsScopedToThePipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")
	other := s.open(t, "other")

	seedStepOutputs(ctx, t, st)

	// The other pipeline can neither read run-1's output nor write to it.
	if _, ok := readOutput(ctx, t, other, "run-1", 1, 0); ok {
		t.Error("a pipeline read the output of another pipeline's run")
	}

	err := other.AppendStepOutput(ctx, "run-1", 1, 0, 9, []byte("intruder"))
	if !errors.Is(err, store.ErrNoSuchRun) {
		t.Errorf("appending to another pipeline's run returned %v, want store.ErrNoSuchRun", err)
	}

	if got, _ := readOutput(ctx, t, st, "run-1", 1, 0); got != "hello world\n" {
		t.Errorf("after the intruder, output = %q", got)
	}
}

// TestStepOutputGoesWithItsRun: nothing sweeps kept output, because reaping the run (run_history:) and destroying the pipeline are what remove it.
func (s suite) TestStepOutputGoesWithItsRun(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	for n := 1; n <= 3; n++ {
		build(ctx, t, st, "build", fmt.Sprintf("RUN%05d", n), n)

		err := st.AppendStepOutput(ctx, fmt.Sprintf("RUN%05d", n), 1, 0, 0, []byte("kept"))
		if err != nil {
			t.Fatalf("AppendStepOutput: %v", err)
		}
	}

	err := st.Prune(ctx, store.Retention{JobName: "build", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for run, want := range map[string]bool{"RUN00001": false, "RUN00002": false, "RUN00003": true} {
		if _, ok := readOutput(ctx, t, st, run, 1, 0); ok != want {
			t.Errorf("%s output present = %v, want %v", run, ok, want)
		}
	}

	err = st.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reopened := s.open(t, "test")
	if _, ok := readOutput(ctx, t, reopened, "RUN00003", 1, 0); ok {
		t.Error("a destroyed pipeline's output is still readable")
	}
}
