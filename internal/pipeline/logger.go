package pipeline

// Which run, job, and step a log line belongs to — carried on the context
// rather than threaded as parameters.
//
// Every function down here already takes a ctx, and the identity a log line
// needs is fixed the moment a run or a step starts. Passing it as arguments
// instead meant a purely diagnostic value dictated the signature of anything
// that might one day log: a retry helper, a resource fetch, a cache lookup.
// The parameters spread exactly as far as the logging did, hook call sites
// had to invent a sentinel index for "not a plan step", and a package that
// wanted only a run id had to grow a dependency to read one.
//
// Stamping inverts that: RunJob names the run and job once, each dispatch
// names its own step, and every line below inherits both without a single
// call site knowing they exist. The logger itself rides on internal/events,
// the leaf packages that cannot import this one already share — so a resource
// fetch several frames down logs under the same step as the walk that asked
// for it.

import (
	"context"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

// withRunLogger stamps the run and job every line under it belongs to. Called
// once per RunJob, before anything can log.
func withRunLogger(ctx context.Context, runID, jobName string) context.Context {
	return events.WithLogger(ctx, events.Logger(ctx).With("run", runID, "job", jobName))
}

// withStepLogger stamps the plan step about to execute, so everything it
// produces — its own lines, its retries, its resource fetches, its agent's
// tool calls — says which step it came from.
//
// Set per dispatch, so the concurrent branches of an in_parallel or an
// across: each carry their own identity rather than sharing the block's.
func withStepLogger(ctx context.Context, i int, step config.Step) context.Context {
	logger := events.Logger(ctx).With("index", i)

	if name := eventStepName(step); name != "" {
		logger = logger.With("step", name)
	}

	if kind := stepKindName(step); kind != "" {
		logger = logger.With("kind", kind)
	}

	return events.WithLogger(ctx, logger)
}

// withHookLogger marks what a hook body produces as the hook's rather than
// the step's it hangs off. A hook holds no plan position, so it carries its
// scope label instead of an index — inventing one would file its output under
// an unrelated step.
func withHookLogger(ctx context.Context, scope, hook string) context.Context {
	return events.WithLogger(ctx, events.Logger(ctx).With("hook", hook, "scope", scope))
}

// logFrom is the logger every line in this package goes through.
func logFrom(ctx context.Context) *slog.Logger {
	return events.Logger(ctx)
}
