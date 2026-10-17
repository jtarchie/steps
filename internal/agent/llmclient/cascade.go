package llmclient

// The cascade's decision, extracted from the loop that acts on it.
//
// This file exists because of how the mid-run cascade kept failing review.
// Every defect found in it was the same shape: the loop chose between three
// outcomes using a conjunction of four independently-computed booleans
// (is the error transient, is it infrastructure, has the step's deadline
// passed, is there another source), and the durable side effect — pinning or
// releasing an agent's source for the life of the PROCESS — rode along on
// whichever branch happened to be taken. Sixteen reachable combinations, no
// name for any of them, and tests that could only ever cover the handful
// somebody thought to write down. A source pinned because it returned 400. A
// healthy pin dropped because the step ran long. Each was a real bug and each
// was invisible until someone traced the conjunction by hand.
//
// So the decision is a value here, computed by a pure function from named
// facts, and cascade_test.go enumerates its whole input space. A combination
// nobody considered is now a failing test rather than a finding.

import (
	"context"
	"errors"

	"github.com/jtarchie/steps/internal/outcome"
)

// SourceOutcome is what one attempt says about the SOURCE that ran it, which
// is a different question from what it says about the step. A step can fail
// on a perfectly healthy source, and a source can be unreachable without the
// step being wrong about anything.
//
// Keeping the two apart is the distinction the cascade previously lacked: it
// pinned on "the step reached a conclusion", which is true of a provider
// answering 400 Bad Request.
type SourceOutcome int

const (
	// sourceServed: this source carried the conversation to a conclusion.
	// Success, a model refusing the task, turn exhaustion, a failed assert,
	// a budget breach, a detected loop — the step has its answer, and the
	// source demonstrably works.
	sourceServed SourceOutcome = iota
	// sourceRefused: the provider answered, and its answer was a permanent
	// rejection of the request (a 4xx that is not worth retrying — an
	// unknown model, an unsupported parameter, a bad key). The step gets
	// that error, because the next source is likely to reject the same
	// request the same way. But this source proved nothing good about
	// itself, so it must not be preferred afterwards.
	sourceRefused
	// sourceUnwell: a transient provider failure — 5xx, a connection that
	// died, an unreachable endpoint. This is the one the cascade exists for.
	sourceUnwell
	// sourceUnproven: neither a conclusion nor evidence against the source.
	// The step's own deadline ran out, or the run was aborted. Nothing here
	// justifies changing which source the process prefers, in either
	// direction.
	sourceUnproven
)

// CascadeAction is what the loop does with the attempt it just finished.
type CascadeAction int

const (
	// ReturnResult means hand this attempt's result back as the step's outcome.
	ReturnResult CascadeAction = iota
	// swapSource: resume this conversation on the next fallback entry.
	swapSource
)

// PinAction is what the attempt does to the process-wide record of which
// source this agent runs on (see selectedSources). It is separate from
// CascadeAction because the two are genuinely independent: a step can end on
// a source worth preferring or one worth forgetting, and the same action can
// accompany either.
type PinAction int

const (
	// LeavePin means no evidence worth acting on. The default, and what every
	// ambiguous case resolves to — a pin is a durable, process-lifetime
	// preference, and changing it on a guess is how a run strands itself on
	// a source the next probe may not get to for a whole poll interval.
	LeavePin PinAction = iota
	// PinThisSource means this source served, so prefer it from here on.
	PinThisSource
	// DropPin means stop preferring the pinned source. Two decisions produce it,
	// for unrelated reasons: DecideCascade, when the cascade tried the
	// alternatives and none served, so the next step should start over from
	// the primary rather than keep preferring whichever was tried last; and
	// DecidePreflightPin, when the pre-run probe says the reason for the pin
	// is gone or the pinned source is.
	DropPin
)

// Verdict is one attempt's decision, whole.
type Verdict struct {
	Action CascadeAction
	Pin    PinAction
}

// ClassifySource decides what runErr says about the source that produced it.
//
// jobCtx is the JOB's context, never the conversation's own: the two produce
// the identical error for opposite reasons, and only the job's says whether
// the run itself is still healthy.
func ClassifySource(jobCtx context.Context, runErr error) SourceOutcome {
	if runErr == nil {
		return sourceServed
	}

	switch outcome.Classify(jobCtx, runErr) {
	case outcome.Aborted:
		// Somebody stopped the run. That is a fact about the operator, not
		// about the endpoint.
		return sourceUnproven
	case outcome.Succeeded, outcome.Failed:
		// A task-level outcome: the conversation ran and the step's own
		// verdict is no.
		return sourceServed
	case outcome.Errored:
	}

	// The step spent its whole deadline without concluding. Whether the
	// endpoint hung or the work was simply too big for the budget is not
	// knowable from here, and the cascade has no time left to investigate
	// either way — the deadline bounds the STEP, so every remaining source
	// would start already expired.
	if errors.Is(runErr, context.DeadlineExceeded) {
		return sourceUnproven
	}

	if isTransientProviderError(runErr) {
		return sourceUnwell
	}

	if isProviderRejection(runErr) {
		return sourceRefused
	}

	// Everything left is this package's own error raised against a source
	// that was answering fine: a budget breach, a response we could not use.
	return sourceServed
}

// isProviderRejection reports whether err is the provider itself declining
// the request in a way another attempt will not fix — the non-retryable
// statuses. It exists to keep such a rejection from being read as this
// source having served the step.
func isProviderRejection(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	return !retryableStatus(apiErr.StatusCode)
}

// DecideCascade is the whole decision, pure and total.
//
//   - hasNext: another fallback entry resolved and its credential is present.
//   - deadlineSpent: the step's shared deadline has already passed, so no
//     later source could finish anything.
//   - swapped: the cascade has already moved off the source it started on.
//
// swapped is what gates DropPin, and it is the correction for the case that
// used to drop a healthy pin: releasing means "I tried the alternatives and
// none served". A step that never swapped has tried no alternatives, so it
// has learned nothing that justifies sending the next step back to a primary
// preflight may already have found dead.
func DecideCascade(result SourceOutcome, hasNext, deadlineSpent, swapped bool) Verdict {
	switch result {
	case sourceServed:
		return Verdict{Action: ReturnResult, Pin: PinThisSource}

	case sourceRefused, sourceUnproven:
		return Verdict{Action: ReturnResult, Pin: LeavePin}

	case sourceUnwell:
		if hasNext && !deadlineSpent {
			return Verdict{Action: swapSource, Pin: LeavePin}
		}

		if swapped {
			return Verdict{Action: ReturnResult, Pin: DropPin}
		}

		return Verdict{Action: ReturnResult, Pin: LeavePin}
	}

	// Unreachable for the constants above; a new SourceOutcome lands here
	// rather than silently taking some other case's branch.
	return Verdict{Action: ReturnResult, Pin: LeavePin}
}

// ProbeFact is what one pre-run probe established: how the source answered,
// and whether that answer postdates the pin it is being weighed against.
//
// Fresh is a property of the ANSWER measured against the PIN, and neither
// half of that is incidental. Not of the caller: several agents may share one
// endpoint and model, the probe cache collapses them to a single request, and
// the ones that read it back hold the same fact as the one that asked. And
// not against a clock of its own: what makes an answer able to end a pin is
// that it was given after the pin existed, since an older one was given
// before the reason for the pin did. See reconsiderPin.
//
// The two travel together because a decision that destroys state may only
// rest on a fresh fact.
type ProbeFact struct {
	Healthy bool
	Fresh   bool
}

// Recovered reports the one fact strong enough to end a pin on its own: this
// source answered, and answered just now.
func (p ProbeFact) Recovered() bool { return p.Healthy && p.Fresh }

// DecidePreflightPin answers the question the mid-run cascade deliberately
// does not: is the reason for this pin still true? Until this existed there
// was no answer at all — an outage lasting ninety seconds could move a
// `steps web` agent onto a possibly-worse model for days, and silently,
// since agent.failover fires once at the swap and nothing afterwards says the
// agent is still there.
//
// Either fact is enough on its own to end a pin: the reason for it is gone,
// or the thing it points at is. Both, because releasing on a recovered
// primary alone would trade one blind spot for a worse one — see
// reconsiderPin, which gathers them.
//
// Each fact must be FRESH to release, and this is the asymmetry the function
// exists to hold: keeping a pin on a stale reading changes nothing, while
// releasing on one destroys evidence a real conversation paid for. A cached
// positive from before an outage would otherwise drop a pin the cascade
// earned during it, and a cached negative from before a recovery would drop
// one a source has since served a whole conversation on — the second
// reachable whenever another job cascades while this probe is in flight,
// which is what clearSourceIf's compare-and-delete already braces for.
//
// Pure and total for the same reason DecideCascade is: every defect this pair
// of functions has closed was a wrong predicate mutating durable state.
func DecidePreflightPin(pinned bool, primary, pinnedSource ProbeFact) PinAction {
	if !pinned {
		// An unhealthy primary with no pin is the ordinary failover path,
		// which is not this function's business.
		return LeavePin
	}

	if primary.Recovered() {
		return DropPin
	}

	if !pinnedSource.Healthy && pinnedSource.Fresh {
		return DropPin
	}

	return LeavePin
}
