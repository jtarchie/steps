package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/jtarchie/steps/internal/outcome"
)

// TestClassifySource pins what an attempt's error says about the SOURCE that
// produced it, which is the distinction the cascade previously did not draw.
//
// The two rows that matter most are the ones that used to be wrong: a 400 is
// the provider rejecting the request, not this source having served the step,
// and a spent step deadline is evidence about the step's budget rather than
// about the endpoint's health.
func TestClassifySource(t *testing.T) {
	t.Parallel()

	// The shape a real connection failure takes reaching here (via net/http's
	// client) — a bare errors.New with dial-shaped TEXT is not a net.Error.
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	tests := []struct {
		name string
		err  error
		want sourceOutcome
	}{
		{"nil error is a conclusion", nil, sourceServed},
		{"a task-level failure is a conclusion", outcome.Fail(errors.New("the model said no")), sourceServed},
		{
			"a task-level failure wrapping a 5xx shape is still a conclusion",
			outcome.Fail(&openai.Error{StatusCode: http.StatusInternalServerError}),
			sourceServed,
		},
		{
			"our own error against a working source is a conclusion",
			errors.New("agent budget exceeded (spent 500 tokens)"),
			sourceServed,
		},

		{"a 5xx is the provider being unwell", &openai.Error{StatusCode: http.StatusInternalServerError}, sourceUnwell},
		{"a connection failure likewise", dialErr, sourceUnwell},

		// The pin bug: this used to fall through to "concluded", so a source
		// that rejected the request outright was recorded as the one to
		// prefer — and nothing ever un-records it, since preflight probes
		// only the primary.
		{"a 400 is the provider refusing", &openai.Error{StatusCode: http.StatusBadRequest}, sourceRefused},
		{"a 404 likewise", &openai.Error{StatusCode: http.StatusNotFound}, sourceRefused},

		// The release bug: a step that merely ran long used to read as
		// evidence against its source and drop a healthy pin.
		{"a spent step deadline proves nothing", context.DeadlineExceeded, sourceUnproven},
		// Wrapped is the shape that actually arrives: the conversation layer
		// returns "generate content: %w" around it.
		{"even wrapped", fmt.Errorf("generate content: %w", context.DeadlineExceeded), sourceUnproven},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := classifySource(t.Context(), test.err); got != test.want {
				t.Errorf("classifySource(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

// TestClassifySourceReadsTheJobContext pins that an aborted RUN is never read
// as anything about the endpoint. The job's context is the only thing that
// can tell "somebody pressed ctrl-c" from "this step's own budget ran out",
// since both arrive as the same error.
func TestClassifySourceReadsTheJobContext(t *testing.T) {
	t.Parallel()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if got := classifySource(canceled, &openai.Error{StatusCode: http.StatusInternalServerError}); got != sourceUnproven {
		t.Errorf("classifySource on a canceled job = %v, want sourceUnproven — an abort says nothing about the provider", got)
	}
}

// TestDecideCascadeIsTotal walks the decision's ENTIRE input space and checks
// each combination against the rules stated as prose, rather than against a
// list of cases somebody thought to write down.
//
// This is the test the cascade was missing. Every defect review found in it
// was a combination that existed but had no case: a source pinned on a 400, a
// pin dropped because a step ran long, a cascade continuing past its own
// deadline. None of those were subtle once named — they were unreachable by
// inspection because the decision was spread across four booleans and three
// return statements with side effects attached.
func TestDecideCascadeIsTotal(t *testing.T) {
	t.Parallel()

	outcomes := map[sourceOutcome]string{
		sourceServed:   "served",
		sourceRefused:  "refused",
		sourceUnwell:   "unwell",
		sourceUnproven: "unproven",
	}

	for result, name := range outcomes {
		for _, hasNext := range []bool{false, true} {
			for _, deadlineSpent := range []bool{false, true} {
				for _, swapped := range []bool{false, true} {
					t.Run(caseName(name, hasNext, deadlineSpent, swapped), func(t *testing.T) {
						t.Parallel()

						got := decideCascade(result, hasNext, deadlineSpent, swapped)
						want := expectedVerdict(result, hasNext, deadlineSpent, swapped)

						if got != want {
							t.Errorf("decideCascade = %+v, want %+v", got, want)
						}
					})
				}
			}
		}
	}
}

// expectedVerdict states the rules independently of decideCascade's own
// control flow, so the two have to agree rather than one paraphrasing the
// other:
//
//   - Only a transient failure cascades, and only while there is somewhere to
//     go and time left to get there.
//   - Only a source that concluded the step is pinned.
//   - A pin is only dropped after alternatives were actually TRIED and none
//     served; a step that never swapped has learned nothing.
func expectedVerdict(result sourceOutcome, hasNext, deadlineSpent, swapped bool) cascadeVerdict {
	if result == sourceServed {
		return cascadeVerdict{action: returnResult, pin: pinThisSource}
	}

	if result != sourceUnwell {
		return cascadeVerdict{action: returnResult, pin: leavePin}
	}

	if hasNext && !deadlineSpent {
		return cascadeVerdict{action: swapSource, pin: leavePin}
	}

	if swapped {
		return cascadeVerdict{action: returnResult, pin: dropPin}
	}

	return cascadeVerdict{action: returnResult, pin: leavePin}
}

func caseName(result string, hasNext, deadlineSpent, swapped bool) string {
	parts := []string{result}

	for _, flag := range []struct {
		on   bool
		text string
	}{
		{hasNext, "hasNext"},
		{deadlineSpent, "deadlineSpent"},
		{swapped, "swapped"},
	} {
		if flag.on {
			parts = append(parts, flag.text)
		}
	}

	return strings.Join(parts, "+")
}

// TestDecideCascadeNeverPinsWhatDidNotServe is the invariant behind the
// pin-on-400 defect, asserted directly rather than left to be inferred from
// the table: pinning is a process-lifetime preference that nothing
// re-examines (preflight probes only the primary), so the bar for writing one
// is that this source actually carried a step to its end.
func TestDecideCascadeNeverPinsWhatDidNotServe(t *testing.T) {
	t.Parallel()

	for _, result := range []sourceOutcome{sourceRefused, sourceUnwell, sourceUnproven} {
		for _, hasNext := range []bool{false, true} {
			for _, deadlineSpent := range []bool{false, true} {
				for _, swapped := range []bool{false, true} {
					if got := decideCascade(result, hasNext, deadlineSpent, swapped); got.pin == pinThisSource {
						t.Errorf("decideCascade(%v, %v, %v, %v) pinned a source that did not serve", result, hasNext, deadlineSpent, swapped)
					}
				}
			}
		}
	}
}

// TestDecideCascadeStopsAtTheDeadline pins the other half: the step's
// deadline bounds the whole cascade, so once it has passed no further source
// is tried. A later source would begin already expired and fail instantly,
// turning one outage into as many failures as the list is long.
func TestDecideCascadeStopsAtTheDeadline(t *testing.T) {
	t.Parallel()

	for _, swapped := range []bool{false, true} {
		got := decideCascade(sourceUnwell, true, true, swapped)
		if got.action == swapSource {
			t.Errorf("decideCascade kept cascading past the step's deadline (swapped=%v)", swapped)
		}
	}
}

// TestDecidePreflightPinIsTotal enumerates the whole input space of the other
// pin decision, for the same reason TestDecideCascadeIsTotal does: a
// preference that outlives runs, changed on a guess, is how a run strands
// itself.
//
// Thirty-two cases rather than eight because each probe now carries whether
// it was actually asked. Freshness is not a detail of one branch — it is a
// second axis over the whole space, and the combinations that changed
// (healthy-but-stale, dead-but-stale) are exactly the ones nobody enumerated
// when they were unrepresentable.
func TestDecidePreflightPinIsTotal(t *testing.T) {
	t.Parallel()

	for _, pinned := range []bool{false, true} {
		for _, primary := range allProbeFacts() {
			for _, pinnedSource := range allProbeFacts() {
				t.Run(pinCaseName(pinned, primary, pinnedSource), func(t *testing.T) {
					t.Parallel()

					got := decidePreflightPin(pinned, primary, pinnedSource)
					want := expectedPinAction(pinned, primary, pinnedSource)

					if got != want {
						t.Errorf("decidePreflightPin(%v, %+v, %+v) = %v, want %v",
							pinned, primary, pinnedSource, got, want)
					}
				})
			}
		}
	}
}

func allProbeFacts() []probeFact {
	return []probeFact{
		{healthy: false, fresh: false},
		{healthy: false, fresh: true},
		{healthy: true, fresh: false},
		{healthy: true, fresh: true},
	}
}

// expectedPinAction states the rules independently of the implementation's
// control flow, so the two have to agree rather than one paraphrasing the
// other:
//
//   - With no pin there is nothing to reconsider.
//   - A recovered primary ends the reason the pin existed.
//   - A dead pinned source has to re-decide too, or fixing the first blind
//     spot just trades it for a worse one.
//   - Neither counts unless it was established just now. Keeping a pin on a
//     stale reading changes nothing; releasing on one throws away what a
//     whole conversation established.
func expectedPinAction(pinned bool, primary, pinnedSource probeFact) pinAction {
	switch {
	case !pinned:
		return leavePin
	case primary.healthy && primary.fresh:
		return dropPin
	case !pinnedSource.healthy && pinnedSource.fresh:
		return dropPin
	default:
		return leavePin
	}
}

func pinCaseName(pinned bool, primary, pinnedSource probeFact) string {
	return fmt.Sprintf("pinned=%v_primary=%v/fresh=%v_pinnedSource=%v/fresh=%v",
		pinned, primary.healthy, primary.fresh, pinnedSource.healthy, pinnedSource.fresh)
}

// TestDecidePreflightPinKeepsAServingFallback is the status quo this must not
// break, asserted directly: while the primary is still down and the fallback
// is still answering, the pin is exactly what should happen — re-deciding
// there is how a flapping primary would oscillate an agent between models.
func TestDecidePreflightPinKeepsAServingFallback(t *testing.T) {
	t.Parallel()

	got := decidePreflightPin(true, probeFact{healthy: false, fresh: true}, probeFact{healthy: true, fresh: true})
	if got != leavePin {
		t.Errorf("a serving fallback under a dead primary got %v, want the pin left alone", got)
	}
}

// TestDecidePreflightPinIgnoresAStaleRecovery is the second defect this
// closes, at the level where it is decidable.
//
// The sequence: t=0 the primary answers and the answer caches. t=1m a
// conversation takes a 5xx mid-run and the fallback serves, earning a pin.
// t=2m the next run's "probe" is a pure cache hit returning t=0's success —
// so the pin was dropped on a question nobody had asked since before the
// outage, the step ran at the still-broken primary, cascaded, re-pinned, and
// repeated every poll until the cache entry aged out.
func TestDecidePreflightPinIgnoresAStaleRecovery(t *testing.T) {
	t.Parallel()

	staleRecovery := probeFact{healthy: true, fresh: false}

	for _, pinnedSource := range allProbeFacts() {
		if !pinnedSource.healthy && pinnedSource.fresh {
			continue // freshly dead: it ends the pin on its own account
		}

		if got := decidePreflightPin(true, staleRecovery, pinnedSource); got != leavePin {
			t.Errorf("a cached positive from before the outage got %v (pinned source %+v), want the pin left alone",
				got, pinnedSource)
		}
	}
}

// TestDecidePreflightPinIgnoresAStaleFailure is the same asymmetry in the
// other direction, and it is reachable whenever two jobs run at once: this
// probe reads a cached negative taken before another job's cascade pinned the
// very source that has since carried a conversation to its end. Dropping the
// pin there discards evidence that a real conversation paid for, on a reading
// older than it.
func TestDecidePreflightPinIgnoresAStaleFailure(t *testing.T) {
	t.Parallel()

	staleFailure := probeFact{healthy: false, fresh: false}

	if got := decidePreflightPin(true, probeFact{healthy: false, fresh: true}, staleFailure); got != leavePin {
		t.Errorf("a cached negative about the pinned source got %v, want the pin left alone", got)
	}
}

// TestDecidePreflightPinNeverTouchesAnUnpinnedAgent guards the one case where
// acting would be a regression rather than a fix: an agent with no pin has
// nothing to return to, and dropPin there would be a write for no reason.
func TestDecidePreflightPinNeverTouchesAnUnpinnedAgent(t *testing.T) {
	t.Parallel()

	for _, primary := range allProbeFacts() {
		for _, pinnedSource := range allProbeFacts() {
			if got := decidePreflightPin(false, primary, pinnedSource); got != leavePin {
				t.Errorf("decidePreflightPin(false, %+v, %+v) = %v, want the pin left alone",
					primary, pinnedSource, got)
			}
		}
	}
}
