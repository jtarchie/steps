package pipeline

import (
	"context"
	"fmt"
	"slices"

	"github.com/jtarchie/steps/internal/config"
)

// execLog is the ordered list of entity names that actually ran during one
// RunJob invocation — task/agent/put/get names and the names of any hook
// steps — in execution order. It backs a job's assert.execution self-check.
// Recording happens only at the pipeline dispatch points (never inside
// internal/agent), so the log lives entirely in this package.
type execLog struct {
	names []string
}

func (l *execLog) record(name string) {
	l.names = append(l.names, name)
}

func (l *execLog) snapshot() []string {
	out := make([]string, len(l.names))
	copy(out, l.names)

	return out
}

type execLogKeyType struct{}

// execLogKey is the context key under which RunJob stashes the per-invocation
// execLog. Unexported and package-local, so nothing outside pipeline can read
// or write it.
var execLogKey = execLogKeyType{} //nolint:gochecknoglobals // zero-size context key sentinel

// withExecLog returns ctx carrying log, so the dispatch points reached through
// it can record without any signature changes.
func withExecLog(ctx context.Context, log *execLog) context.Context {
	return context.WithValue(ctx, execLogKey, log)
}

// recordExecution appends name to the execLog carried by ctx, if any. It is a
// no-op when ctx carries no log (every caller outside a RunJob assert context —
// trigger, unit tests — is unaffected).
func recordExecution(ctx context.Context, name string) {
	log, ok := ctx.Value(execLogKey).(*execLog)
	if !ok {
		return
	}

	log.record(name)
}

// recordStepExecution records step's name, except for a try: wrapper — the
// step it wraps records itself, under the same name (executedStepName looks
// through the wrapper), so logging both would double-count one execution in a
// job's assert.execution. Recording only the wrapper is not an option: it would
// claim an execution for a when:-guarded inner step that never ran.
func recordStepExecution(ctx context.Context, step config.Step) {
	if step.Try != nil {
		return
	}

	// An in_parallel: block records nothing of its own either: it is a
	// container, and its branches record themselves — in declaration order,
	// not completion order (see runBranches), so assert.execution stays a
	// deterministic thing to write.
	if step.InParallel != nil || step.Race != nil || step.Ensemble != nil {
		return
	}

	recordExecution(ctx, executedStepName(step))
}

// forkExecLog returns a context carrying a fresh, empty execution log, plus
// that log. It lets concurrent branches record independently and have their
// entries merged back in a defined order.
func forkExecLog(ctx context.Context) (context.Context, *execLog) {
	forked := &execLog{}

	return withExecLog(ctx, forked), forked
}

// mergeExecLog appends another log's entries to the one in ctx.
//
// This is what keeps assert.execution usable with in_parallel: branches finish
// in whatever order they finish, so recording as they go would make the log —
// and therefore any assert on it — nondeterministic. Merging per branch, in
// declaration order, gives back a stable sequence that a fixture can name.
func mergeExecLog(ctx context.Context, from *execLog) {
	for _, name := range from.snapshot() {
		recordExecution(ctx, name)
	}
}

// checkExecution compares the names that actually ran (got) against the
// ordered names an assert.execution requires (want), returning nil on an exact
// match and an error naming the ordered diff otherwise.
func checkExecution(label string, want, got []string) error {
	if slices.Equal(want, got) {
		return nil
	}

	return fmt.Errorf("%s: assert.execution mismatch:\n  want: %v\n  got:  %v", label, want, got)
}

// checkJobAssert evaluates a job's assert: against what ran (log) and what the
// plan concluded (planErr), returning the error the job should now carry: nil
// when every assertion held, otherwise the assertion failure or the plan's own
// error. An assertion failure is never cleared.
//
// The two directives answer different questions and compose:
//
//	execution:  which steps ran, in what order
//	outcome:    what the plan CONCLUDED
//
// A matching execution: clears whatever the plan produced, which is what lets a
// fixture of deliberately-failing tasks be green. That clearing is also why
// execution: alone cannot express "this job should have failed": both branches
// of an error-propagation defect run the same steps, so the assert matches
// either way and then erases the very difference under test. outcome: is the
// missing half — and outcome: succeeded is not a no-op, it opts out of the
// clearing so a plan failure stays a failure.
//
// Absent outcome:, this is byte-identical to the behavior that predates it.
func checkJobAssert(job *config.Job, log *execLog, planErr error) error {
	if job.Assert == nil {
		return planErr
	}

	label := fmt.Sprintf("job %q", job.Name)

	if len(job.Assert.Execution) > 0 {
		err := checkExecution(label, job.Assert.Execution, log.snapshot())
		if err != nil {
			return err
		}
	}

	switch job.Assert.Outcome {
	case config.AssertOutcomeFailed:
		if planErr == nil {
			return fmt.Errorf("%s: assert.outcome: failed, but the plan succeeded", label)
		}

		// The failure was the assertion: satisfying it is what makes the job
		// green, exactly as a matching execution: does.
		return nil

	case config.AssertOutcomeSucceeded:
		if planErr != nil {
			return fmt.Errorf("%s: assert.outcome: succeeded, but the plan failed: %w", label, planErr)
		}

		return nil

	default: // absent
		if len(job.Assert.Execution) > 0 {
			return nil
		}

		return planErr
	}
}
