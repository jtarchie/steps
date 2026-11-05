package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/outcome"
)

// An abort is the machinery saying no: Errored, so on_error fires, and never
// read as somebody pressing ctrl-c — which is what the cancellation it rides
// on would otherwise look like at the process boundary.
func TestAnAbortedStepIsErroredNotAborted(t *testing.T) {
	ctx, settle := withStepAbort(t.Context())

	refusal := errors.New(`mcp server "tracker" refused read_items`)
	if !abortStep(ctx, refusal) {
		t.Fatal("abortStep found nothing to abort under withStepAbort")
	}

	if ctx.Err() == nil {
		t.Fatal("the abort did not cancel the conversation's context")
	}

	err := settle(context.Canceled)
	if !errors.Is(err, refusal) || errors.Is(err, context.Canceled) {
		t.Errorf("settled = %v, want the refusal in place of the cancellation", err)
	}

	if class := outcome.Classify(t.Context(), err); class != outcome.Errored {
		t.Errorf("classified %v, want Errored", class)
	}

	if code := outcome.ExitCode(err); code != outcome.ExitErrored {
		t.Errorf("exit code %d, want %d", code, outcome.ExitErrored)
	}
}

// Anything that is not an abort passes through settle untouched, including a
// cancellation from above, which IS somebody stopping the run.
func TestSettleLeavesEverythingElseAlone(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	_, settle := withStepAbort(parent)

	want := errors.New("the model said no")

	got := settle(want)
	if !errors.Is(got, want) {
		t.Errorf("settle(%v) = %v with nothing aborted", want, got)
	}

	cancel()

	got = settle(context.Canceled)
	if !errors.Is(got, context.Canceled) {
		t.Errorf("a cancelled parent settled to %v, want the cancellation kept", got)
	}

	if abortStep(t.Context(), want) {
		t.Error("abortStep claimed a context with no withStepAbort above it")
	}
}
