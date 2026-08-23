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
			RunID:        event.RunID,
			Type:         event.Type,
			StepIndex:    event.StepIndex,
			StepName:     event.StepName,
			StepKind:     event.StepKind,
			StepID:       event.StepID,
			ParentStepID: event.ParentStepID,
			Status:       event.Status,
			Hash:         event.Hash,
			Text:         event.Text,
			Name:         event.Name,
			Detail:       event.Detail,
			DurationMS:   event.DurationMS,
			Worker:       event.Worker,
			At:           event.At,
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

// publishStepStarted announces a step that is about to execute, and returns
// the mark identifying it — which its finish event reports again, and which a
// container hands to what it runs (see steptree.go).
func publishStepStarted(ctx context.Context, jobName string, i int, step config.Step) stepMark {
	mark := markStep(ctx)

	events.Publish(ctx, events.Event{
		Type:         events.TypeStepStarted,
		RunID:        runIDFrom(ctx),
		Job:          jobName,
		StepIndex:    i,
		StepName:     eventStepName(step),
		StepKind:     stepKindName(step),
		StepID:       mark.id,
		ParentStepID: mark.parent,
	})

	return mark
}

// publishStepFinished announces how a step ended, with the hash it produced
// so a consumer can link straight to the node.
//
// It is also the one place a step's completion reaches slog — an operator
// reading `--log-level` output, not the event bus/web UI, otherwise saw a
// step START (the dispatchers' own "job.step" Debug line) and never learned
// how long it ran or whether it succeeded.
func publishStepFinished(
	ctx context.Context, jobName string, i int, step config.Step, mark stepMark,
	hash string, started time.Time, err error,
) {
	status := "succeeded"
	text := ""

	if err != nil {
		status = string(outcome.Classify(ctx, err))
		text = err.Error()
	}

	logFrom(ctx).Info("job.step.finished", "status", status, "duration", time.Since(started))

	events.Publish(ctx, events.Event{
		Type:         events.TypeStepFinished,
		RunID:        runIDFrom(ctx),
		Job:          jobName,
		StepIndex:    i,
		StepName:     eventStepName(step),
		StepKind:     stepKindName(step),
		StepID:       mark.id,
		ParentStepID: mark.parent,
		Status:       status,
		Hash:         hash,
		Text:         text,
		DurationMS:   time.Since(started).Milliseconds(),
		Worker:       placementOf(ctx, step),
	})
}

// publishStepSkipped announces a step that did not execute, carrying WHY —
// a merkle cache hit, a when: guard, or a resume. The reason is the whole
// value of the event: "nothing happened" is not actionable, "replayed from
// cache" is.
//
// mark is the caller's, not one minted here, because a skip is not always the
// first anyone hears of the step: a when: guard and an in-place get both
// publish step_started before they know the step will be skipped, and the
// context inside such a step already names that start as the container. A
// fresh mark there closes nothing and nests the skip under its own start, so
// the consumer holds an entry that never ends. A step nobody announced — a
// chain skip — passes markStep(ctx) itself.
func publishStepSkipped(
	ctx context.Context, jobName string, i int, step config.Step, mark stepMark, hash, reason string,
) {
	events.Publish(ctx, events.Event{
		Type:         events.TypeStepSkipped,
		RunID:        runIDFrom(ctx),
		Job:          jobName,
		StepIndex:    i,
		StepName:     eventStepName(step),
		StepKind:     stepKindName(step),
		StepID:       mark.id,
		ParentStepID: mark.parent,
		Status:       "skipped",
		Hash:         hash,
		Text:         reason,
	})
}

// stepIdentityKey carries which plan step is currently executing, so the
// frames that actually hold a command's output can publish it without five
// intermediate signatures growing a parameter to carry the index down. It is
// the same context-threading the package already uses for resume state, the
// execution log, and the force flag.
type stepIdentityKey struct{}

type stepIdentity struct {
	job   string
	index int
	step  config.Step
}

// withStepIdentity tags ctx with the step about to run. Set per dispatch, so
// concurrent branches of an in_parallel or across each carry their own.
func withStepIdentity(ctx context.Context, jobName string, i int, step config.Step) context.Context {
	return context.WithValue(ctx, stepIdentityKey{}, stepIdentity{job: jobName, index: i, step: step})
}

// withHookIdentity tags ctx for a hook body: the job it belongs to, but no
// plan position.
//
// Both halves matter, and each was previously wrong in a different
// direction. A hook inherited whatever stepIdentity its enclosing step had
// left behind, so a STEP-level hook's fix agent published under the step's
// own index — the identity currentStepRef's contract says a hook must not
// claim. And a JOB-level hook inherited nothing at all, so its fix agent
// published under an empty job name, filing a real conversation under no job
// in the browser.
func withHookIdentity(ctx context.Context, jobName string) context.Context {
	return context.WithValue(ctx, stepIdentityKey{}, stepIdentity{job: jobName, index: -1})
}

// currentStepRef is which plan step is executing, for the frames that must
// hand that identity to another package rather than merely log it — a fix
// agent's conversation publishes events under the step that invoked it (see
// agent.RunFix).
//
// index -1 means there is no plan position: a hook or a fix running outside
// the plan walk. Inventing one would file the conversation under an
// unrelated step. The JOB is still reported in that case — it is known, and
// losing it files the conversation under nothing at all.
func currentStepRef(ctx context.Context) (jobName string, index int) {
	identity, ok := ctx.Value(stepIdentityKey{}).(stepIdentity)
	if !ok {
		return "", -1
	}

	return identity.job, identity.index
}

// publishOutputForCurrentStep publishes a command's output against whichever
// step the context says is running, and does nothing at all when no run put
// an identity on the context.
//
// It does NOT currently skip a hook, though currentStepRef's contract says a
// hook holds no plan position: withHookIdentity installs an identity with
// index -1 and a zero Step, so a hook task's output publishes with an empty
// name and kind and the run view files it under the enclosing step (or under
// a nameless one). Pre-dates the fix:/assert: work and affects the plain task
// path identically; fixing it means deciding whether a hook gets a step
// identity of its own or publishes nothing, which is a DSL call.
func publishOutputForCurrentStep(ctx context.Context, stdout, stderr string) {
	identity, ok := ctx.Value(stepIdentityKey{}).(stepIdentity)
	if !ok {
		return
	}

	publishStepOutput(ctx, identity.job, identity.index, identity.step, stdout, stderr)
}

// maxPublishedOutputBytes bounds what one step contributes to a run's event
// log. Generous enough for the output a person actually reads — 32KB, the
// same bound a tool result gets inline, so a failing command's tail is not
// cut shorter in the UI than the model itself would have seen — while a
// runaway command still cannot turn the transcript into a copy of its own
// stdout.
const maxPublishedOutputBytes = 32_000

// publishStepOutput records what a step printed, whichever way the step ended.
//
// Especially when it failed. Nothing else carries a failing command's output:
// the error a task returns is "command %q failed: exit status N", and an
// assert mismatch names the expectation rather than the output that missed
// it. (taskFailureOutput does fold output into text, but only into the prompt
// runFixTask hands the fix agent — it never reaches the transcript.) A step
// that printed nothing publishes nothing: an empty log block is worse than
// no log block.
//
// Callers arrive with different budgets and that is NOT reconciled here. The
// plain task path bounds its own capture (RunStreamedCapture takes
// maxPublishedOutputBytes per stream, and appends its own "... [truncated N
// bytes]" marker); assert:/fix: tasks capture with the unbounded
// RunCaptureFull because assert.stdout has to see the whole stream, so their
// rows are bounded only by store.MaxEventTextBytes. A cap applied here cannot
// fix that without also clipping the marker off an already-compliant stream —
// the budget belongs in runCaptured, which is where the capture happens.
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
		StepID:    events.StepID(ctx),
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
	case stepCacheHit:
		return "same inputs as an earlier run — outputs reused"
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
	// across: is a modifier rather than a kind, so Kind() reports what each
	// CELL will be — but the step publishing here is the block, which is what
	// dispatchNonGetStep resolves it to before it ever asks for a kind. Left
	// as the cell kind, a matrix and its cells were three rows all labelled
	// "try", with nothing on the page saying which one was the matrix.
	if len(step.Across) > 0 {
		return "across"
	}

	kind, ok := step.Kind()
	if !ok {
		return ""
	}

	return string(kind)
}
