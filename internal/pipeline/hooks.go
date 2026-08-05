package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/workspace"
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
	cfg     *config.Config
	jobName string
	label   string
	bw      workspace.BuildWorkspace
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
		logIfHookFailed(scope, "on_failure", runMatchedHook(ctx, scope, "on_failure", hooks.OnFailure))
	case outcome.Errored:
		logIfHookFailed(scope, "on_error", runMatchedHook(ctx, scope, "on_error", hooks.OnError))
	case outcome.Aborted:
		logIfHookFailed(scope, "on_abort", runMatchedHook(ctx, scope, "on_abort", hooks.OnAbort))
	}

	ensureErr := runMatchedHook(ctx, scope, "ensure", hooks.Ensure)

	// The guarded work's own failure always wins and is returned unchanged;
	// ensure's failure on that path is only logged. On a green outcome,
	// on_success's failure takes precedence over ensure's.
	if baseErr != nil {
		logIfHookFailed(scope, "ensure", ensureErr)

		return baseErr
	}

	if promoted != nil {
		logIfHookFailed(scope, "ensure", ensureErr)

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
	slog.Debug("job.hook", "scope", scope.label, "hook", name)

	hookErr := runHookStep(hookCtx, scope, *step)

	nested := hookScope{
		cfg:     scope.cfg,
		jobName: scope.jobName,
		label:   fmt.Sprintf("%s (%s hook)", scope.label, name),
		bw:      scope.bw,
	}

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

		return executeTask(ctx, scope.cfg, step, rt, scope.bw)
	case config.StepKindPut:
		_, err := executePut(ctx, scope.cfg, step, scope.bw)

		return err
	case config.StepKindAgent:
		err := agent.RunHook(ctx, scope.cfg, step, scope.bw)
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
	case config.StepKindInParallel:
		return runInParallelHookStep(ctx, scope, step)
	default: // config.StepKindGet — not a valid hook body
		return errors.New("unrecognized hook step (must be task, put, or agent)")
	}
}

func logIfHookFailed(scope hookScope, name string, err error) {
	if err != nil {
		slog.Warn("job.hook.failed", "scope", scope.label, "hook", name, "error", err.Error())
	}
}

// runInParallelHookStep executes an in_parallel hook step by running its
// children concurrently, respecting limit and fail_fast, and collecting
// errors.
func runInParallelHookStep(ctx context.Context, scope hookScope, step config.Step) error {
	spec := step.InParallel

	children := spec.Steps
	limit := spec.Limit
	if limit <= 0 || limit > len(children) {
		limit = len(children)
	}

	type result struct {
		index int
		err   error
	}

	// Suppress automatic execution logging during concurrent execution —
	// children would record in non-deterministic completion order. We
	// re-record in declaration order after all complete.
	noLogCtx := context.WithValue(ctx, execLogKey, nil)

	// When fail_fast is set, derive a cancellable context so the first
	// child failure cancels still-running siblings.
	childCtx, cancel := context.WithCancel(noLogCtx)
	defer cancel()

	sem := make(chan struct{}, limit)
	results := make(chan result, len(children))

	for i, child := range children {
		sem <- struct{}{}
		go func(idx int, c config.Step) {
			defer func() { <-sem }()
			label := fmt.Sprintf("%s (branch %d)", scope.label, idx)
			err := runHookStep(childCtx, hookScope{cfg: scope.cfg, jobName: scope.jobName, label: label, bw: scope.bw}, c)
			results <- result{index: idx, err: err}
		}(i, child)
	}

	var firstErr error
	for range children {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			if spec.FailFast {
				cancel()
			}
		}
	}

	// Record children in declaration order for deterministic assert.execution.
	for _, child := range children {
		recordStepExecution(ctx, child)
	}

	return firstErr
}

// executedStepName is the bare name of whatever a task/agent/put step ran,
// recorded into the execution log for a job's assert.execution. get steps
// record their resource name separately (see runTriggeredBuild).
func executedStepName(step config.Step) string {
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
	case config.StepKindTry:
		return executedStepName(*step.Try)
	case config.StepKindInParallel:
		return ""
	default:
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
	case config.StepKindInParallel:
		return fmt.Sprintf("step %d (in_parallel)", i)
	default: // config.StepKindTask, or a malformed step — label as a task, as before
		return fmt.Sprintf("step %d (task %q)", i, step.Task)
	}
}
