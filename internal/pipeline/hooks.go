package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

// hookGracePeriod bounds how long on_abort and ensure hooks may run after the
// job's context has already been canceled (a SIGINT/SIGTERM mid-run): they run
// detached from the canceled context — the same context.WithoutCancel idiom
// internal/trigger uses to finalize an interrupted job — but not forever.
const hookGracePeriod = 60 * time.Second

// hookScope is the context one set of hooks runs in: the config to resolve
// against, the job name, a human-readable label for logging (e.g.
// `job "deploy"` or `step 2 (task "build")`), and the build workspace hook
// steps materialize into.
type hookScope struct {
	stepRunner

	label string
}

// runHooks dispatches the Concourse-style hooks around a guarded outcome:
// on_success/on_failure/on_error/on_abort/ensure, matching Concourse's five
// hook step modifiers and their firing conditions exactly — see
// docs/conformance.md, TestRunHooksRouting and TestRunHooksAbortGracePeriod
// in hooks_test.go, and concourse-ci.org/docs/steps/ ("Hooks"/modifier
// pages).
// baseErr is the guarded work's own result (nil = success). It classifies
// baseErr once (against the job context), runs the single matching on_* hook,
// then always runs ensure last. Hooks are observers: a non-nil baseErr is
// returned unchanged and any hook's own failure is only logged. On a green
// outcome, a failing on_success or ensure hook becomes the returned error
// (on_success wins over ensure), so a broken notification or cleanup can't
// leave a build falsely green.
func runHooks(ctx context.Context, scope hookScope, hooks config.Hooks, baseErr error) error {
	if hooks.Empty() {
		return baseErr
	}

	var promoted error

	switch outcome.Classify(ctx, baseErr) {
	case outcome.Succeeded:
		promoted = runMatchedHook(ctx, scope, "on_success", hooks.OnSuccess)
	case outcome.Failed:
		logIfHookFailed(ctx, scope, "on_failure", runMatchedHook(ctx, scope, "on_failure", hooks.OnFailure))
	case outcome.Errored:
		logIfHookFailed(ctx, scope, "on_error", runMatchedHook(ctx, scope, "on_error", hooks.OnError))
	case outcome.Aborted:
		logIfHookFailed(ctx, scope, "on_abort", runMatchedHook(ctx, scope, "on_abort", hooks.OnAbort))
	}

	ensureErr := runMatchedHook(ctx, scope, "ensure", hooks.Ensure)

	// The guarded work's own failure always wins and is returned unchanged;
	// ensure's failure on that path is only logged. On a green outcome,
	// on_success's failure takes precedence over ensure's.
	if baseErr != nil {
		logIfHookFailed(ctx, scope, "ensure", ensureErr)

		return baseErr
	}

	if promoted != nil {
		logIfHookFailed(ctx, scope, "ensure", ensureErr)

		return promoted
	}

	return ensureErr
}

// runMatchedHook runs one hook step (a no-op for a nil hook), returning its
// outcome. It detaches a grace context when the job context is already
// canceled so on_abort/ensure still run to completion, then recurses so the
// hook's own hooks observe the hook's outcome.
func runMatchedHook(ctx context.Context, scope hookScope, name string, step *config.Step) error {
	if step == nil {
		return nil
	}

	hookCtx := ctx

	if ctx.Err() != nil {
		var cancel context.CancelFunc

		hookCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), hookGracePeriod)
		defer cancel()
	}

	fmt.Printf("%s: %s hook\n", scope.label, name)

	// Everything the hook body runs logs as the HOOK's, not as the step it
	// hangs off — a hook has no plan position of its own, and filing its
	// output under the step's index would read as the step doing it. The
	// identity is re-tagged for the same reason the logger is: without it a
	// hook inherits the enclosing step's plan position (or, on a job-level
	// hook, no job at all) and publishes a fix agent's conversation there.
	hookCtx = withHookLogger(hookCtx, scope.label, name)
	hookCtx = withHookIdentity(hookCtx, scope.jobName)
	logFrom(hookCtx).Debug("job.hook")

	// Built BEFORE the body runs, and handed to it. The label is the identity
	// a placement is recorded under, and a hook has no node hash to be keyed
	// on instead — so the enclosing step's label made an on_failure: and an
	// ensure: on one step the same key, the second upserting over the first.
	// Two machines were acquired and billed and one of them vanished from the
	// record, silently, because an upsert is a success.
	nested := scope.scope(fmt.Sprintf("%s (%s hook)", scope.label, name))

	hookErr := runHookStep(hookCtx, nested, *step)

	return runHooks(hookCtx, nested, step.Hooks, hookErr)
}

// runHookStep executes one hook step body — a task, put, or agent — with NO
// merkle hashing and NO store recording (the same no-record contract as a
// task's fix: agent): the enclosing step or job records the aggregate outcome.
// get is never a valid hook (rejected at LoadConfig), so it is not handled.
func runHookStep(ctx context.Context, scope hookScope, step config.Step) error {
	recordStepExecution(ctx, step)

	kind, ok := step.Kind()
	if !ok {
		return errors.New("unrecognized hook step (must be task, put, or agent)")
	}

	switch kind { //nolint:exhaustive // default covers config.StepKindGet, not a valid hook body
	case config.StepKindTask:
		rt, err := scope.cfg.ResolveTask(step)
		if err != nil {
			return fmt.Errorf("task %q: %w", step.Task, err)
		}

		// A hook carrying tags: acquires a machine like any other placed step
		// — on an aws://launch/ worker it launches and bills an instance — and
		// this is the one place an operator would least expect one to be
		// running. Keyed on the scope's label rather than a node hash, because
		// a hook has no node: it must never be skipped for having succeeded
		// before, so it is deliberately outside the merkle chain.
		ctx, placed := withPlacementSink(ctx)

		err = executeTask(ctx, scope.cfg, step, rt, scope.bw, scope.st)

		recordPlacement(ctx, scope.stepRunner, placed, 0, executedStepName(step), scope.label, "")

		return err
	case config.StepKindPut:
		// Recorded on the same terms as a task hook, keyed on the scope's
		// label because a hook has no node.
		ctx, placed := withPlacementSink(ctx)

		_, err := executePut(ctx, scope.cfg, step, scope.bw)

		recordPlacement(ctx, scope.stepRunner, placed, 0, step.Put, scope.label, "")

		return err
	case config.StepKindAgent:
		err := agent.RunHook(ctx, scope.cfg, scope.jobName, step, scope.bw, scope.st)
		if err != nil {
			return fmt.Errorf("agent hook: %w", err)
		}

		return nil
	case config.StepKindTry:
		// A try: hook body tolerates its own failure, exactly as it does in a
		// plan. Without the toleration a `ensure: {try: {put: notify}}` — the
		// use docs/control-flow.md advertises — still turned a green build red
		// via runHooks' promotion of a failed on_success/ensure hook.
		return tolerateTryFailure(ctx, scope.jobName, step, runHookStep(ctx, scope, *step.Try))
	default: // config.StepKindGet — not a valid hook body
		return errors.New("unrecognized hook step (must be task, put, or agent)")
	}
}

func logIfHookFailed(ctx context.Context, scope hookScope, name string, err error) {
	if err != nil {
		logFrom(ctx).Warn("job.hook.failed", "scope", scope.label, "hook", name, "error", err.Error())
	}
}

// executedStepName is the bare name of whatever a task/agent/put step ran,
// recorded into the execution log for a job's assert.execution. get steps
// record their resource name separately (see runTriggeredBuild).
func executedStepName(step config.Step) string {
	// Mirrors config.stepName: a computed Label is the step's identity when it
	// has one, so a matrix cell is reported and recorded under the name it is
	// known by rather than the one it resolves through.
	if step.Label != "" {
		return step.Label
	}

	kind, ok := step.Kind()
	if !ok {
		return ""
	}

	switch kind { //nolint:exhaustive // default covers config.StepKindGet, which records its resource name separately
	case config.StepKindTask:
		return step.Task
	case config.StepKindAgent:
		return step.Agent
	case config.StepKindPut:
		return step.Put
	case config.StepKindLoadVar:
		// The var it captures IS its name. Without this the step recorded an
		// empty string, which put a bare "" in the middle of every
		// assert.execution covering a job that loads a var — unwritable as a
		// fixture, and indistinguishable from a step that recorded nothing.
		return step.LoadVar
	case config.StepKindTry:
		return executedStepName(*step.Try)
	default:
		// Every remaining kind is unnamed by construction: a get records its
		// resource name separately, an approval has only a message, and the
		// container kinds never reach here (recordStepExecution returns
		// early — their children record themselves).
		return ""
	}
}

// stepLabel builds a hook scope's label for a plan step.
func stepLabel(i int, step config.Step) string {
	kind, _ := step.Kind()

	switch kind { //nolint:exhaustive // default covers config.StepKindTask and a malformed step alike, both labeled as a task as before
	case config.StepKindGet:
		return fmt.Sprintf("step %d (get %q)", i, step.Get)
	case config.StepKindPut:
		return fmt.Sprintf("step %d (put %q)", i, step.Put)
	case config.StepKindAgent:
		return fmt.Sprintf("step %d (agent %q)", i, step.Agent)
	case config.StepKindTry:
		return fmt.Sprintf("step %d (try %q)", i, executedStepName(*step.Try))
	default: // config.StepKindTask, or a malformed step — label as a task, as before
		return fmt.Sprintf("step %d (task %q)", i, step.Task)
	}
}
