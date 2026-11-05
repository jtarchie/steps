package agent

// A tool call that no next turn can fix ends the step instead of being handed
// to the model. toolImpl's contract is "failure is data", and for almost
// every failure that is right: a model can fix its own arguments. A server
// refusing the credential for want of a scope is the exception — no argument
// changes it, and a model told about it as data spends turns (and, with
// required: or assert.files:, re-wakes) retrying a call only a login fixes.

import (
	"context"

	"github.com/jtarchie/steps/internal/outcome"
)

type stepAbortKey struct{}

// withStepAbort gives a conversation a way to be ended from inside a tool
// call, and settle turns what the conversation then returned into the step's
// error: the abort's cause, as Errored, in place of whatever cancellation
// error the conversation unwound with.
//
// A cancellation, not a returned error, because the tool call is several
// layers below the loop that would have to return it, and for a CLI agent it
// runs in another goroutine serving another process's request. Cancelling the
// conversation's context reaches both: the API loop stops at its next
// request, and a CLI subprocess is killed. The bridge's requests keep this
// context's values (clibridge.go's BaseContext), which is how a CLI agent's
// tool call finds abortStep's hook.
func withStepAbort(ctx context.Context) (context.Context, func(error) error) {
	ctx, cancel := context.WithCancelCause(ctx)
	aborted := ctx

	ctx = context.WithValue(ctx, stepAbortKey{}, cancel)

	return ctx, func(err error) error {
		cause := context.Cause(aborted)
		if aborted.Err() == nil || cause == context.Canceled { //nolint:errorlint // context.Cause returns this exact value for a plain cancel
			cancel(nil)

			return err
		}

		cancel(nil)

		return outcome.Escalate(cause)
	}
}

// abortStep ends the conversation ctx belongs to with cause, and reports
// whether there was one to end. A conversation with no withStepAbort above it
// (a resource's MCP backend, a test calling a tool directly) gets false, and
// the caller falls back to reporting the failure as data.
func abortStep(ctx context.Context, cause error) bool {
	cancel, ok := ctx.Value(stepAbortKey{}).(context.CancelCauseFunc)
	if !ok {
		return false
	}

	cancel(cause)

	return true
}
