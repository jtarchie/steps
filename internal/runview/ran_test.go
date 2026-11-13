package runview

import (
	"reflect"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

func ranOf(turns []Turn) []time.Duration {
	out := make([]time.Duration, len(turns))
	for i, turn := range turns {
		out[i] = turn.Ran
	}

	return out
}

// Hosted results publish only after the whole batch ran, so the batch is the unit, drawn once so it never reads doubled.
func TestTheFirstResultOfABatchCarriesHowLongItsToolsRan(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentUser, 0, 0),
		conversationRow(2, events.TypeAgentCall, 2*time.Second, 0),
		conversationRow(3, events.TypeAgentResult, 274*time.Second, 0),
		conversationRow(4, events.TypeAgentCall, 280*time.Second, 0),
		conversationRow(5, events.TypeAgentCall, 280*time.Second, 0),
		conversationRow(6, events.TypeAgentResult, 283*time.Second, 0),
		conversationRow(7, events.TypeAgentResult, 283*time.Second, 0),
		conversationRow(8, events.TypeAgentText, 290*time.Second, 0),
	})

	want := []time.Duration{0, 0, 272 * time.Second, 0, 0, 3 * time.Second, 0, 0}
	if got := ranOf(step.Turns); !reflect.DeepEqual(got, want) {
		t.Errorf("Ran = %v, want %v", got, want)
	}
}

// A sub-agent tool runs for the child's whole conversation; the child's calls are timed at their own depth.
func TestADelegationRunsUntilItsResult(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentCall, 0, 0),
		conversationRow(2, events.TypeAgentUser, time.Second, 1),
		conversationRow(3, events.TypeAgentCall, 2*time.Second, 1),
		conversationRow(4, events.TypeAgentResult, 5*time.Second, 1),
		conversationRow(5, events.TypeAgentResult, 9*time.Second, 0),
	})

	want := []time.Duration{0, 0, 0, 3 * time.Second, 9 * time.Second}
	if got := ranOf(step.Turns); !reflect.DeepEqual(got, want) {
		t.Errorf("Ran = %v, want %v", got, want)
	}
}

// A garbage duration is worse than none.
func TestAnUntimeableResultCarriesNothing(t *testing.T) {
	t.Parallel()

	orphan := foldTurns(t, []store.RunEventRow{conversationRow(1, events.TypeAgentResult, time.Second, 0)})

	zeroCall := conversationRow(1, events.TypeAgentCall, 0, 0)
	zeroCall.At = time.Time{}
	untimed := foldTurns(t, []store.RunEventRow{zeroCall, conversationRow(2, events.TypeAgentResult, time.Second, 0)})

	backwards := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentCall, 5*time.Second, 0),
		conversationRow(2, events.TypeAgentResult, time.Second, 0),
	})

	for name, step := range map[string]*Step{"orphan": orphan, "untimed": untimed, "backwards": backwards} {
		for _, turn := range step.Turns {
			if turn.Ran != 0 {
				t.Errorf("%s: turn %s Ran = %v, want nothing", name, turn.Type, turn.Ran)
			}
		}
	}
}

// The live stream folds row by row; a call and its result straddling two batches must time the same.
func TestRanDoesNotDependOnHowTheFoldWasBatched(t *testing.T) {
	t.Parallel()

	rows := []store.RunEventRow{
		{Seq: 0, Type: events.TypeStepStarted, StepID: 1, StepName: "review", StepKind: "agent", At: tookT0},
		conversationRow(1, events.TypeAgentCall, 0, 0),
		conversationRow(2, events.TypeAgentResult, 90*time.Second, 0),
	}

	whole := NewFolder()
	whole.Add(rows, nil)

	split := NewFolder()
	for _, row := range rows {
		split.Add([]store.RunEventRow{row}, nil)
	}

	a, b := ranOf(whole.Steps()[0].Turns), ranOf(split.Steps()[0].Turns)
	if !reflect.DeepEqual(a, b) || b[1] != 90*time.Second {
		t.Errorf("batched Ran = %v, split Ran = %v, want 1m30s on the result either way", a, b)
	}
}
