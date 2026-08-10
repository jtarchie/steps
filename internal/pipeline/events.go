package pipeline

// Publishing what a run is doing, as it does it — see internal/events.
//
// The pipeline already knew all of this; it just told only the terminal. Each
// helper here is a no-op unless something put a bus on the context, so
// `steps run` in a shell behaves exactly as it did before this file existed.

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
)

// StoreSink returns the bus sink that persists a run's events.
//
// It lives here, not in the web layer, because recording what a run did is
// the RUNNER's job. A run's story must not depend on who happened to be
// watching: a job started from a terminal and the same job started from the
// UI have to leave the same record, or "open the run that failed last night"
// works only for runs that were being watched at the time.
func StoreSink(st *store.Store) func(events.Event) {
	return func(event events.Event) {
		err := st.AppendRunEvent(context.Background(), store.RunEventRow{
			RunID:      event.RunID,
			JobName:    event.Job,
			Type:       event.Type,
			StepIndex:  event.StepIndex,
			StepName:   event.StepName,
			StepKind:   event.StepKind,
			Status:     event.Status,
			Hash:       event.Hash,
			Text:       event.Text,
			Name:       event.Name,
			Detail:     event.Detail,
			DurationMS: event.DurationMS,
			At:         event.At,
		})
		if err != nil {
			slog.Warn("run.event_persist", "type", event.Type, "error", err)
		}
	}
}

// attachEventBus makes sure the run publishes somewhere. A caller that
// already installed a bus (the web server, which also fans events out to live
// viewers) keeps theirs; everyone else gets a recording-only bus, so the run
// is readable afterwards regardless of how it was started.
//
// The returned func must be called when the run ends: it drains the sink so
// the last events land before the process moves on.
func attachEventBus(ctx context.Context, st *store.Store) (context.Context, func()) {
	if events.FromContext(ctx) != nil {
		return ctx, func() {}
	}

	bus := events.New(StoreSink(st))

	return events.WithBus(ctx, bus), bus.Close
}

// runIDFrom returns the current run's id, or "" when there is no resume state
// (a hook or fix conversation running outside a plan walk).
func runIDFrom(ctx context.Context) string {
	resume := resumeFrom(ctx)
	if resume == nil {
		return ""
	}

	return resume.id
}

// publishStepStarted announces a step that is about to execute.
func publishStepStarted(ctx context.Context, jobName string, i int, step config.Step) {
	events.Publish(ctx, events.Event{
		Type:      events.TypeStepStarted,
		RunID:     runIDFrom(ctx),
		Job:       jobName,
		StepIndex: i,
		StepName:  eventStepName(step),
		StepKind:  stepKindName(step),
	})
}

// publishStepFinished announces how a step ended, with the hash it produced
// so a consumer can link straight to the node.
func publishStepFinished(ctx context.Context, jobName string, i int, step config.Step, hash string, started time.Time, err error) {
	status := "succeeded"
	text := ""

	if err != nil {
		status = string(outcome.Classify(ctx, err))
		text = err.Error()
	}

	events.Publish(ctx, events.Event{
		Type:       events.TypeStepFinished,
		RunID:      runIDFrom(ctx),
		Job:        jobName,
		StepIndex:  i,
		StepName:   eventStepName(step),
		StepKind:   stepKindName(step),
		Status:     status,
		Hash:       hash,
		Text:       text,
		DurationMS: time.Since(started).Milliseconds(),
	})
}

// publishStepSkipped announces a step that did not execute, carrying WHY —
// a merkle cache hit, a when: guard, or a resume. The reason is the whole
// value of the event: "nothing happened" is not actionable, "replayed from
// cache" is.
func publishStepSkipped(ctx context.Context, jobName string, i int, step config.Step, hash, reason string) {
	events.Publish(ctx, events.Event{
		Type:      events.TypeStepSkipped,
		RunID:     runIDFrom(ctx),
		Job:       jobName,
		StepIndex: i,
		StepName:  eventStepName(step),
		StepKind:  stepKindName(step),
		Status:    "skipped",
		Hash:      hash,
		Text:      reason,
	})
}

// stepIdentityKey carries which plan step is currently executing, so the
// frames that actually hold a command's output can publish it without five
// intermediate signatures growing a parameter to carry the index down. It is
// the same context-threading the package already uses for resume state, the
// execution log, and the force flag.
type stepIdentityKey struct{}

type stepIdentity struct {
	index int
	step  config.Step
}

// withStepIdentity tags ctx with the step about to run. Set per dispatch, so
// concurrent branches of an in_parallel or across each carry their own.
func withStepIdentity(ctx context.Context, i int, step config.Step) context.Context {
	return context.WithValue(ctx, stepIdentityKey{}, stepIdentity{index: i, step: step})
}

// publishOutputForCurrentStep publishes a command's output against whichever
// step the context says is running. A no-op off the plan walk — a hook or a
// fix command has no plan index, and inventing one would attach its output to
// an unrelated step.
func publishOutputForCurrentStep(ctx context.Context, jobName, stdout, stderr string) {
	identity, ok := ctx.Value(stepIdentityKey{}).(stepIdentity)
	if !ok {
		return
	}

	publishStepOutput(ctx, jobName, identity.index, identity.step, stdout, stderr)
}

// maxPublishedOutputBytes bounds what one step contributes to a run's event
// log. Generous enough for the output a person actually reads, small enough
// that a runaway command cannot turn the transcript into a copy of its own
// stdout.
const maxPublishedOutputBytes = 16_000

// publishStepOutput records what a step printed, whichever way the step ended.
//
// Especially when it failed. Nothing else carries a failing command's output:
// the error a task returns is "command %q failed: exit status N", and an
// assert mismatch names the expectation rather than the output that missed
// it. (taskFailureOutput does fold output into text, but only into the prompt
// runFixTask hands the fix agent — it never reaches the transcript.) A step
// that printed nothing publishes nothing: an empty log block is worse than
// no log block.
func publishStepOutput(ctx context.Context, jobName string, i int, step config.Step, stdout, stderr string) {
	combined := strings.TrimRight(stdout, "\n")

	if trimmed := strings.TrimRight(stderr, "\n"); trimmed != "" {
		if combined != "" {
			combined += "\n"
		}

		combined += trimmed
	}

	if combined == "" {
		return
	}

	events.Publish(ctx, events.Event{
		Type:      events.TypeStepOutput,
		RunID:     runIDFrom(ctx),
		Job:       jobName,
		StepIndex: i,
		StepName:  eventStepName(step),
		StepKind:  stepKindName(step),
		Text:      combined,
	})
}

// publishJobStarted / publishJobFinished bracket a whole run.
func publishJobStarted(ctx context.Context, jobName string) {
	events.Publish(ctx, events.Event{
		Type:      events.TypeJobStarted,
		RunID:     runIDFrom(ctx),
		Job:       jobName,
		StepIndex: -1,
	})
}

func publishJobFinished(ctx context.Context, jobName string, started time.Time, err error) {
	status := "succeeded"
	text := ""

	if err != nil {
		status = string(outcome.Classify(ctx, err))
		text = err.Error()
	}

	events.Publish(ctx, events.Event{
		Type:       events.TypeJobFinished,
		RunID:      runIDFrom(ctx),
		Job:        jobName,
		StepIndex:  -1,
		Status:     status,
		Text:       text,
		DurationMS: time.Since(started).Milliseconds(),
	})
}

// skipReason turns a non-running disposition into the words a reader needs.
// "skipped" alone is the least useful thing a transcript can say — a
// when: guard and a merkle cache hit look identical and mean opposite things.
func skipReason(disposition stepDisposition) string {
	switch disposition {
	case stepChainSkipped:
		return "unchanged — replayed from cache"
	case stepGuardSkipped:
		return "when: guard was false"
	case stepRan:
		return ""
	}

	return ""
}

// eventStepName is the name an event reports a step under.
//
// It is executedStepName plus a name for GET steps, which that function
// deliberately leaves blank — the execution log records a get by its resource
// separately, but an event has only this one field, and a transcript of
// unnamed steps is unreadable. A get is known by the artifact it produces
// (step.Get), which is also what the plan's later steps refer to it as.
func eventStepName(step config.Step) string {
	if name := executedStepName(step); name != "" {
		return name
	}

	if step.Get != "" {
		return step.Get
	}

	return step.Resource
}

// stepKindName is the step's kind as a display string, falling back to the
// empty string for a step whose kind does not resolve (already an error on
// the execution path; here it just means the event says less).
func stepKindName(step config.Step) string {
	kind, ok := step.Kind()
	if !ok {
		return ""
	}

	return string(kind)
}
