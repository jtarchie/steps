package runview

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

var tookT0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// conversationRow is one agent event of step 1, at t0+offset and depth.
func conversationRow(seq int64, eventType string, offset time.Duration, depth int) store.RunEventRow {
	row := store.RunEventRow{
		Seq: seq, Type: eventType, StepID: 1, StepName: "review", StepKind: "agent",
		At: tookT0.Add(offset),
	}
	if depth > 0 {
		row.Status = fmt.Sprintf("depth:%d", depth)
	}

	return row
}

func tookOf(turns []Turn) []time.Duration {
	out := make([]time.Duration, len(turns))
	for i, turn := range turns {
		out[i] = turn.Took
	}

	return out
}

func foldTurns(t *testing.T, rows []store.RunEventRow) *Step {
	t.Helper()

	opening := store.RunEventRow{Seq: 0, Type: events.TypeStepStarted, StepID: 1, StepName: "review", StepKind: "agent", At: tookT0}
	view := Build(store.RunRow{ID: "R"}, append([]store.RunEventRow{opening}, rows...), nil)

	return view.Steps[0]
}

// TestOnlyTheFirstModelTurnOfAResponseCarriesItsRequest pins which turns are
// timed: the text and calls of one response share a request, results and
// messages are not the model's.
func TestOnlyTheFirstModelTurnOfAResponseCarriesItsRequest(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentSystem, 0, 0),
		conversationRow(2, events.TypeAgentUser, time.Second, 0),
		conversationRow(3, events.TypeAgentText, 3*time.Second, 0),
		conversationRow(4, events.TypeAgentCall, 3100*time.Millisecond, 0),
		conversationRow(5, events.TypeAgentResult, 4*time.Second, 0),
		conversationRow(6, events.TypeAgentCall, 136*time.Second, 0),
		conversationRow(7, events.TypeAgentResult, 137*time.Second, 0),
		conversationRow(8, events.TypeAgentText, 140*time.Second, 0),
	})

	want := []time.Duration{0, 0, 2 * time.Second, 0, 0, 132 * time.Second, 0, 3 * time.Second}
	if got := tookOf(step.Turns); !reflect.DeepEqual(got, want) {
		t.Errorf("Took = %v, want %v", got, want)
	}

	if sent := step.Turns[5].Sent(); !sent.Equal(tookT0.Add(4 * time.Second)) {
		t.Errorf("Sent = %v, want the result that sent the request", sent)
	}
}

// TestASubAgentIsTimedAgainstItsOwnDepth pins depth isolation: a sub-agent
// measures from its own opening, and the parent after the delegate's result
// measures from that result rather than from the sub-agent's traffic.
func TestASubAgentIsTimedAgainstItsOwnDepth(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentUser, 0, 0),
		conversationRow(2, events.TypeAgentCall, time.Second, 0),
		conversationRow(3, events.TypeAgentUser, 2*time.Second, 1),
		conversationRow(4, events.TypeAgentText, 7*time.Second, 1),
		conversationRow(5, events.TypeAgentResult, 8*time.Second, 0),
		conversationRow(6, events.TypeAgentText, 20*time.Second, 0),
	})

	want := []time.Duration{0, time.Second, 0, 5 * time.Second, 0, 12 * time.Second}
	if got := tookOf(step.Turns); !reflect.DeepEqual(got, want) {
		t.Errorf("Took = %v, want %v", got, want)
	}
}

// TestACompactionIsARequestAndABoundary pins the summary call timed on its
// own, and the request after it timed from it rather than absorbing it.
func TestACompactionIsARequestAndABoundary(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentResult, 0, 0),
		conversationRow(2, events.TypeAgentCompaction, 30*time.Second, 0),
		conversationRow(3, events.TypeAgentText, 34*time.Second, 0),
	})

	want := []time.Duration{0, 30 * time.Second, 4 * time.Second}
	if got := tookOf(step.Turns); !reflect.DeepEqual(got, want) {
		t.Errorf("Took = %v, want %v", got, want)
	}
}

// TestASystemPromptAloneOpensARequest covers a conversation that opens with
// no user message.
func TestASystemPromptAloneOpensARequest(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentSystem, 0, 0),
		conversationRow(2, events.TypeAgentText, 6*time.Second, 0),
	})

	if got := step.Turns[1].Took; got != 6*time.Second {
		t.Errorf("Took = %v, want 6s from the system prompt", got)
	}
}

// TestAnUntimeableTurnCarriesNothing covers the turns that must draw nothing
// rather than a garbage number: no request before them, a missing
// timestamp on either side, and a clock that stepped backwards.
func TestAnUntimeableTurnCarriesNothing(t *testing.T) {
	t.Parallel()

	noBoundary := foldTurns(t, []store.RunEventRow{conversationRow(1, events.TypeAgentText, time.Second, 0)})

	zeroAt := conversationRow(2, events.TypeAgentText, 0, 0)
	zeroAt.At = time.Time{}
	zeroTurn := foldTurns(t, []store.RunEventRow{conversationRow(1, events.TypeAgentUser, 0, 0), zeroAt})

	zeroSent := conversationRow(1, events.TypeAgentUser, 0, 0)
	zeroSent.At = time.Time{}
	zeroBoundary := foldTurns(t, []store.RunEventRow{zeroSent, conversationRow(2, events.TypeAgentText, time.Second, 0)})

	backwards := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentUser, 5*time.Second, 0),
		conversationRow(2, events.TypeAgentText, time.Second, 0),
	})

	for name, step := range map[string]*Step{
		"no boundary": noBoundary, "zero turn": zeroTurn, "zero boundary": zeroBoundary, "backwards": backwards,
	} {
		for _, turn := range step.Turns {
			if turn.Took != 0 {
				t.Errorf("%s: turn %s Took = %v, want nothing", name, turn.Type, turn.Took)
			}
		}
	}
}

// TestSlowIsMoreThanATenthOfTheTimeout pins the threshold's edge and that no
// known timeout never warns.
func TestSlowIsMoreThanATenthOfTheTimeout(t *testing.T) {
	t.Parallel()

	step := Step{Timeout: 10 * time.Minute}
	if step.Slow(time.Minute) {
		t.Error("exactly a tenth is slow")
	}

	if !step.Slow(time.Minute + time.Millisecond) {
		t.Error("just over a tenth is not slow")
	}

	if (Step{}).Slow(time.Hour) {
		t.Error("a step with no known timeout warned")
	}
}

// TestAnswerTurnIsTheTurnConversationDropped keeps the final request's time
// reachable after the dedup hides its turn.
func TestAnswerTurnIsTheTurnConversationDropped(t *testing.T) {
	t.Parallel()

	const answer = "done"

	step := Step{
		Result: map[string]any{"response": answer},
		Turns: []Turn{
			{Type: events.TypeAgentResult},
			{Type: events.TypeAgentText, Text: answer, Took: 9 * time.Second},
		},
	}

	if got := step.AnswerTurn(); got.Took != 9*time.Second || got.Text != answer {
		t.Errorf("AnswerTurn = %+v, want the dropped answer turn", got)
	}

	step.Result = map[string]any{"response": "something else"}
	if got := step.AnswerTurn(); got != (Turn{}) {
		t.Errorf("AnswerTurn = %+v with nothing dropped, want the zero Turn", got)
	}
}

// TestUnansweredIsTheRequestAFailedStepEndedOn covers the number a timed-out
// step is opened for, and every step that must not claim one.
func TestUnansweredIsTheRequestAFailedStepEndedOn(t *testing.T) {
	t.Parallel()

	finish := func(status string) store.RunEventRow {
		return store.RunEventRow{
			Seq: 99, Type: events.TypeStepFinished, StepID: 1, StepName: "review", StepKind: "agent",
			Status: status, At: tookT0.Add(10 * time.Minute),
		}
	}

	waiting := []store.RunEventRow{
		conversationRow(1, events.TypeAgentUser, 0, 0),
		conversationRow(2, events.TypeAgentCall, time.Second, 0),
		conversationRow(3, events.TypeAgentUser, 90*time.Second, 1),
		conversationRow(4, events.TypeAgentResult, 140*time.Second, 0),
	}

	failed := foldTurns(t, append(waiting[:len(waiting):len(waiting)], finish("failed")))

	got := failed.Unanswered()
	if got.Took != 10*time.Minute-140*time.Second || got.Depth != 0 {
		t.Errorf("Unanswered = %+v, want the newest pending request (depth 0, 7m40s)", got)
	}

	answered := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentUser, 0, 0),
		conversationRow(2, events.TypeAgentText, time.Second, 0),
		finish("failed"),
	})

	for name, step := range map[string]*Step{
		"running":   foldTurns(t, waiting),
		"succeeded": foldTurns(t, append(waiting[:len(waiting):len(waiting)], finish("succeeded"))),
		"answered":  answered,
	} {
		if got := step.Unanswered(); got != (Turn{}) {
			t.Errorf("%s: Unanswered = %+v, want the zero Turn", name, got)
		}
	}
}

// TestTookDoesNotDependOnHowTheFoldWasBatched is the live stream's promise:
// one Add per row times every turn the same as one Add for the lot.
func TestTookDoesNotDependOnHowTheFoldWasBatched(t *testing.T) {
	t.Parallel()

	rows := []store.RunEventRow{
		{Seq: 0, Type: events.TypeStepStarted, StepID: 1, StepName: "review", StepKind: "agent", At: tookT0},
		conversationRow(1, events.TypeAgentUser, 0, 0),
		conversationRow(2, events.TypeAgentCall, 2*time.Second, 0),
		conversationRow(3, events.TypeAgentResult, 3*time.Second, 0),
		conversationRow(4, events.TypeAgentText, 50*time.Second, 0),
	}

	whole := NewFolder()
	whole.Add(rows, nil)

	split := NewFolder()
	for _, row := range rows {
		split.Add([]store.RunEventRow{row}, nil)
	}

	if a, b := tookOf(whole.Steps()[0].Turns), tookOf(split.Steps()[0].Turns); !reflect.DeepEqual(a, b) {
		t.Errorf("batched Took = %v, split Took = %v", a, b)
	}
}

// TestADeadSubAgentIsNotBlamedForItsParentsEnd: a sub-agent that failed
// mid-request never answers it, and once the parent has spoken again that
// request is over — a step then timed out in a tool was not waiting on it.
func TestADeadSubAgentIsNotBlamedForItsParentsEnd(t *testing.T) {
	t.Parallel()

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentUser, 0, 0),
		conversationRow(2, events.TypeAgentCall, time.Second, 0),
		conversationRow(3, events.TypeAgentUser, 2*time.Second, 1),
		conversationRow(4, events.TypeAgentResult, 30*time.Second, 0),
		conversationRow(5, events.TypeAgentCall, 40*time.Second, 0),
		{
			Seq: 6, Type: events.TypeStepFinished, StepID: 1, StepName: "review", StepKind: "agent",
			Status: "failed", At: tookT0.Add(10 * time.Minute),
		},
	})

	if got := step.Unanswered(); got != (Turn{}) {
		t.Errorf("Unanswered = %+v, want nothing: the step ended in a tool, not on the dead sub-agent", got)
	}
}

// TestUnansweredDoesNotDependOnMapOrder: a request sent at the instant the
// step ended is the newest, and it was in flight for nothing — never a
// coin-flip with the older one beside it.
func TestUnansweredDoesNotDependOnMapOrder(t *testing.T) {
	t.Parallel()

	step := Step{
		Status:  "failed",
		Ended:   tookT0.Add(10 * time.Minute),
		pending: map[int]time.Time{0: tookT0, 1: tookT0.Add(10 * time.Minute)},
	}

	for range 64 {
		if got := step.Unanswered(); got != (Turn{}) {
			t.Fatalf("Unanswered = %+v, want nothing: the newest request was sent as the step ended", got)
		}
	}
}

// TestAnUntimedBoundaryIsNotTimedFromTheOneBeforeIt: a message with no
// timestamp is still what the request went out after, so the reply is
// untimed rather than billed from the boundary before it.
func TestAnUntimedBoundaryIsNotTimedFromTheOneBeforeIt(t *testing.T) {
	t.Parallel()

	user := conversationRow(2, events.TypeAgentUser, 0, 0)
	user.At = time.Time{}

	step := foldTurns(t, []store.RunEventRow{
		conversationRow(1, events.TypeAgentSystem, 0, 0),
		user,
		conversationRow(3, events.TypeAgentText, 30*time.Second, 0),
	})

	if got := step.Turns[2].Took; got != 0 {
		t.Errorf("Took = %v, want nothing: the request's send time is unknown", got)
	}
}
