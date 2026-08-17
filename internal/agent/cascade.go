package agent

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

	"github.com/openai/openai-go/v3"

	"github.com/jtarchie/steps/internal/outcome"
)

// sourceOutcome is what one attempt says about the SOURCE that ran it, which
// is a different question from what it says about the step. A step can fail
// on a perfectly healthy source, and a source can be unreachable without the
// step being wrong about anything.
//
// Keeping the two apart is the distinction the cascade previously lacked: it
// pinned on "the step reached a conclusion", which is true of a provider
// answering 400 Bad Request.
type sourceOutcome int

const (
	// sourceServed: this source carried the conversation to a conclusion.
	// Success, a model refusing the task, turn exhaustion, a failed assert,
	// a budget breach, a detected loop — the step has its answer, and the
	// source demonstrably works.
	sourceServed sourceOutcome = iota
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

// cascadeAction is what the loop does with the attempt it just finished.
type cascadeAction int

const (
	// returnResult: hand this attempt's result back as the step's outcome.
	returnResult cascadeAction = iota
	// swapSource: resume this conversation on the next fallback entry.
	swapSource
)

// pinAction is what the attempt does to the process-wide record of which
// source this agent runs on (see selectedSources). It is separate from
// cascadeAction because the two are genuinely independent: a step can end on
// a source worth preferring or one worth forgetting, and the same action can
// accompany either.
type pinAction int

const (
	// leavePin: no evidence worth acting on. The default, and what every
	// ambiguous case resolves to — a pin is a durable, process-lifetime
	// preference, and changing it on a guess is how a run strands itself on
	// a source nothing will re-examine.
	leavePin pinAction = iota
	// pinThisSource: this source served, so prefer it from here on.
	pinThisSource
	// dropPin: the cascade tried alternatives and none of them served, so
	// the next step should start over from the agent's primary rather than
	// keep preferring whichever source was tried last.
	dropPin
)

// cascadeVerdict is one attempt's decision, whole.
type cascadeVerdict struct {
	action cascadeAction
	pin    pinAction
}

// classifySource decides what runErr says about the source that produced it.
//
// jobCtx is the JOB's context, never the conversation's own: the two produce
// the identical error for opposite reasons, and only the job's says whether
// the run itself is still healthy.
func classifySource(jobCtx context.Context, runErr error) sourceOutcome {
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
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return false
	}

	return !retryableStatus(apiErr.StatusCode)
}

// decideCascade is the whole decision, pure and total.
//
//   - hasNext: another fallback entry resolved and its credential is present.
//   - deadlineSpent: the step's shared deadline has already passed, so no
//     later source could finish anything.
//   - swapped: the cascade has already moved off the source it started on.
//
// swapped is what gates dropPin, and it is the correction for the case that
// used to drop a healthy pin: releasing means "I tried the alternatives and
// none served". A step that never swapped has tried no alternatives, so it
// has learned nothing that justifies sending the next step back to a primary
// preflight may already have found dead.
func decideCascade(result sourceOutcome, hasNext, deadlineSpent, swapped bool) cascadeVerdict {
	switch result {
	case sourceServed:
		return cascadeVerdict{action: returnResult, pin: pinThisSource}

	case sourceRefused, sourceUnproven:
		return cascadeVerdict{action: returnResult, pin: leavePin}

	case sourceUnwell:
		if hasNext && !deadlineSpent {
			return cascadeVerdict{action: swapSource, pin: leavePin}
		}

		if swapped {
			return cascadeVerdict{action: returnResult, pin: dropPin}
		}

		return cascadeVerdict{action: returnResult, pin: leavePin}
	}

	// Unreachable for the constants above; a new sourceOutcome lands here
	// rather than silently taking some other case's branch.
	return cascadeVerdict{action: returnResult, pin: leavePin}
}
