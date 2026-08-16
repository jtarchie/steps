package pipeline

// Walking a plan: deciding what each step is, running it, and advancing.

import (
	"context"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// planWalk is one pass over a list of plan steps: the runner they execute
// through, what a get step needs to resolve and fan out with, and the position
// the walk advances. It is passed BY VALUE into runSteps, so a triggered
// build's remainder walks its own copy.
type planWalk struct {
	stepRunner

	pinned    map[string]string
	provider  workspace.Provider
	skippable map[string]bool
	cache     *rsrc.Cache
	cursor    *versionCursor

	// resolution is the run's input sets — computed once, before planning,
	// and the only thing fanOutGet fans out over.
	resolution setResolution

	// assigned is the current triggered build's input set: which version
	// every get in the remainder binds. Nil outside a triggered build.
	assigned merkle.InputSet

	// allowGetTrigger is false inside a triggered build's remainder, where a
	// get fetches into the existing workspace instead of fanning out again.
	allowGetTrigger bool

	index            int
	parentHash       string
	chainUnskippable bool

	// visits counts how many times each step index has executed this walk,
	// bounding a to:-driven backward loop. Per-walk, so each triggered build
	// (and each version under get: version: every) gets its own max_visits
	// budget.
	visits map[int]int
}

// runSteps executes steps in order. A `get` step fans out: for each version
// it selects (a single version normally, or every version returned by check
// when version:every is set), that version triggers its own build of the
// remainder of the plan — see runTriggeredBuild. It always terminates this
// loop, since it delegates the rest of the plan to its triggered build(s).
// A `task`/`put`/`agent` step is handled by runNonGetStep; `put`/`agent`
// steps are never looked up in skippable and always execute.
func runSteps(ctx context.Context, w planWalk, steps []config.Step) error {
	w.index, w.visits = 0, map[int]int{}

	// A manual index loop (not range) so a to: transition can set the next
	// index — forward to skip ahead, or backward to loop.
	for w.index < len(steps) {
		// Between steps, never during one: the step that was running has
		// finished and kept its work, and this decides only whether another
		// starts. See deadline.go.
		err := jobDeadlinePassed(ctx, w.jobName)
		if err != nil {
			return err
		}

		step := steps[w.index]

		switch {
		case step.Get != "" && !w.allowGetTrigger:
			done, err := w.fetchInPlace(ctx, step, steps)
			if done {
				return err
			}
		case step.Get != "":
			return w.fanOutGet(ctx, step, steps[w.index+1:])
		default:
			done, err := w.runStep(ctx, step, steps)
			if done {
				return err
			}
		}
	}

	// Once more after the last step. The check above runs BEFORE a step, so a
	// deadline that passed during the final one — or during a fan-out that
	// stopped admitting cells because of it — would otherwise never be looked
	// at again, and the job would report success having overrun.
	err := jobDeadlinePassed(ctx, w.jobName)
	if err != nil {
		return err
	}

	return recordChainSucceeded(ctx, w.stepRunner, w.parentHash, w.chainUnskippable)
}

// runStep runs one non-get (task/put/agent) step through execution, routing
// and chain-unskippable folding, advancing the walk. It returns done=true to
// end the walk — with a nil error for a chain skip.
func (w *planWalk) runStep(ctx context.Context, step config.Step, steps []config.Step) (bool, error) {
	// A step this run already finished is skipped outright. This is narrower
	// than the merkle cache and answerable where the cache is not: an agent
	// step is never content-cacheable, but "this run already ran it" is a
	// fact, and re-running it would produce a DIFFERENT answer rather than
	// the reviewed one.
	//
	// It lives HERE, in the plan walk, and not in runNonGetStep — which the
	// concurrent block runners also call, with the enclosing BLOCK's index.
	// Recorded from there, one succeeding branch marked the whole block done,
	// and a resume skipped the block including the branch that had failed,
	// exiting 0 having re-attempted nothing.
	if w.skipCompleted(ctx) {
		return false, nil
	}

	res, err := runNonGetStep(ctx, w.stepRunner, w.index, step, w.skippable, w.parentHash)

	// A cache hit counts as a visit for the same reason it advances
	// parentHash: the step produced its result. (No cacheable step routes, so
	// nothing reads this count today — but a visit that means "the plan moved
	// through here" must not depend on whether the work was paid for twice.)
	if res.disposition == stepRan || res.disposition == stepCacheHit {
		w.visits[w.index]++
	}

	recordCompletedStep(ctx, w.st, w.index, step, err)

	nextIndex, _, err, exhaustedErr := applyRouting(ctx, steps, w.index, step, res.disposition, res.verdict, err, w.visits)
	if exhaustedErr != nil {
		return true, exhaustedErr
	}

	// Last, so a try: wrapper's to: has already routed on the real outcome and
	// the wrapper's hooks have already observed it.
	err = tolerateTryFailure(ctx, w.jobName, step, err)
	if err != nil {
		return true, err
	}

	if res.disposition == stepChainSkipped {
		reportChainSkipped(ctx, w.jobName, w.index+1, steps[w.index+1:])

		return true, nil
	}

	unskippable, err := foldStepUnskippable(ctx, w.cfg, step, w.chainUnskippable)
	if err != nil {
		return true, err
	}

	w.chainUnskippable = unskippable

	if res.disposition != stepGuardSkipped && res.hash != "" {
		w.parentHash = res.hash
	}

	w.index = nextIndex

	return false, nil
}

// skipCompleted skips a plan step a previous attempt of this run already
// finished, advancing the index. It reports whether it skipped.
func (w *planWalk) skipCompleted(ctx context.Context) bool {
	name, done := resumeFrom(ctx).alreadyDone(w.index)
	if !done {
		return false
	}

	fmt.Printf("skip: %s (already succeeded)\n", name)
	logFrom(ctx).Info("job.skip", "index", w.index, "reason", "resume", "step", name)
	recordExecution(ctx, name)

	w.index++

	return true
}

// reportChainSkipped announces the steps a chain-skip swallowed. firstIndex is
// the plan index of the first of them, so each is published under the position
// it holds in the plan.
//
// They are published, not only printed: a transcript that shows the step where
// the cache hit and then simply STOPS reads as a truncated run. It only names
// steps from their config — it never resolves a get step's version or does any
// of the work runSteps is skipping, so it can't reintroduce the resource
// checks caching exists to avoid.
func reportChainSkipped(ctx context.Context, jobName string, firstIndex int, steps []config.Step) {
	for offset, step := range steps {
		name := eventStepName(step)
		if name == "" {
			continue
		}

		fmt.Printf("skip: %s (chain)\n", name)
		logFrom(ctx).Info("job.skip", "index", firstIndex+offset, "step", name, "reason", "chain")
		publishStepSkipped(ctx, jobName, firstIndex+offset, step, "", skipReason(stepChainSkipped))
	}
}

// recordStepOutcome registers an agent step's decision under the name it is
// known by, so a later step that declared context: from: can be handed it.
func recordStepOutcome(ctx context.Context, step config.Step, out agent.StepOutcome) {
	if out.Verdict == "" {
		return
	}

	agent.RecordOutcome(ctx, executedStepName(step), agent.Upstream{Verdict: out.Verdict, Note: out.Note, Response: out.Response})
}

// recordCompletedStep marks a plan step as one a resume will not repeat.
//
// On success only: a failed step is exactly the one a resume must run again.
// And on PLAN steps only — the concurrent block runners call runNonGetStep
// with the enclosing block's index, so recording from there marked a whole
// block done the moment any one branch succeeded.
func recordCompletedStep(ctx context.Context, st *store.Store, i int, step config.Step, err error) {
	if err != nil {
		return
	}

	resume := resumeFrom(ctx)
	if resume == nil {
		return
	}

	_ = st.RecordRunStep(context.WithoutCancel(ctx), resume.id, i, executedStepName(step))
}

// runNonGetStep runs a task/put/agent step and dispatches its hooks around the
// outcome. A skipped (merkle-cached) step fires no hooks — it did not run, so
// there is nothing for its observers to react to. When a green step is failed
// by its own on_success/ensure hook, the true (failed) outcome is recorded so
// the store and skip-cache reflect it.
func runNonGetStep(ctx context.Context, r stepRunner, i int, step config.Step, skippable map[string]bool, parentHash string) (stepResult, error) {
	started := time.Now()

	publishStepStarted(ctx, r.jobName, i, step)

	// Carried so the frames that hold a command's captured output can publish
	// it against the right step (see withStepIdentity).
	ctx = withStepIdentity(ctx, r.jobName, i, step)
	ctx = withStepLogger(ctx, i, step)

	res, err := dispatchNonGetStep(ctx, r, i, step, skippable, parentHash)

	// A step whose outputs were reused counts as executed, even though no
	// command or conversation ran: its artifacts are in place and the plan
	// continued exactly as if it had run, so a fixture's assert.execution must
	// read the same on a cold cache and a warm one. (A resume-skipped step
	// records itself for the same reason — see skipCompleted. A when: guard
	// and a chain skip do not, because there the step genuinely produced
	// nothing.)
	//
	// Recorded before hooks so a job's assert.execution reads
	// [step, its hooks...].
	if res.disposition == stepRan || res.disposition == stepCacheHit {
		recordStepExecution(ctx, step)
	}

	// A skip and a run are different events, not one event with a flag: the
	// whole point of the transcript is that a replayed step is visibly
	// distinct from a step that paid to execute.
	if res.disposition == stepRan {
		publishStepFinished(ctx, r.jobName, i, step, res.hash, started, err)
	} else {
		publishStepSkipped(ctx, r.jobName, i, step, res.hash, skipReason(res.disposition))
	}

	// Neither kind of skip fires hooks: the step did not run, so it has no
	// outcome for its observers to react to.
	if res.disposition != stepRan || step.Hooks.Empty() {
		return res, err
	}

	final := runHooks(ctx, r.scope(stepLabel(i, step)), step.Hooks, err)
	if err == nil && final != nil {
		_ = r.st.RecordJobRun(context.WithoutCancel(ctx), r.jobName, res.hash, string(outcome.Failed), final)

		return stepResult{verdict: res.verdict, note: res.note}, final
	}

	return res, final
}

// dispatchNonGetStep dispatches a task/put/agent step — the three kinds that,
// unlike get, run in place and return a single new parentHash rather than
// fanning out or delegating the remainder of the plan. It first evaluates the
// step's when: guard (see evaluateStepGuard): a false guard skips only this
// step. stepChainSkipped is only ever returned for a cache-matched task step;
// put/agent steps are never chain-skippable.
func dispatchNonGetStep(ctx context.Context, r stepRunner, i int, step config.Step, skippable map[string]bool, parentHash string) (stepResult, error) {
	shouldRun, err := evaluateStepGuard(ctx, r.cfg, step, r.bw)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (when): %w", i, err)
	}

	if !shouldRun {
		fmt.Printf("skip: %s (when)\n", executedStepName(step))
		logFrom(ctx).Info("job.skip", "reason", "when", "step", executedStepName(step))

		return stepResult{hash: parentHash, disposition: stepGuardSkipped}, nil
	}

	// A captured load_var: value changes what a step runs, so substitute
	// before anything hashes or executes it.
	step = renderStepVars(ctx, step)

	switch {
	case step.LoadVar != "":
		return runLoadVarStep(ctx, r, i, step, parentHash)
	case step.Approval != nil:
		return runApprovalStep(ctx, r, i, step, parentHash)
	case len(step.Across) > 0:
		// across: is a MODIFIER, not a kind: the step it sits on is still a
		// task (or a put, or an agent), it just runs once per combination.
		// Checking it before resolving the kind is what keeps it off
		// Step.Kind()'s table, where it would read as a second kind on every
		// step that uses it.
		return runAcrossStep(ctx, r, i, step, parentHash)
	}

	kind, ok := step.Kind()
	if !ok {
		return stepResult{}, unrecognizedStep(i)
	}

	return dispatchByKind(ctx, r, kind, i, step, skippable, parentHash)
}

// dispatchByKind routes a resolved non-get step to its runner. Split from the
// guard evaluation above purely to stay inside the complexity budget; the
// kinds it handles are the whole non-get set, which `exhaustive` enforces.
func dispatchByKind(
	ctx context.Context, r stepRunner, kind config.StepKind, i int, step config.Step,
	skippable map[string]bool, parentHash string,
) (stepResult, error) {
	switch kind { //nolint:exhaustive // default covers config.StepKindGet — this is only called for non-get steps
	case config.StepKindTask:
		return runTaskStep(ctx, r, i, step, skippable, parentHash)
	case config.StepKindPut:
		return runPutStep(ctx, r, i, step, parentHash)
	case config.StepKindAgent:
		return runAgentStep(ctx, r, i, step, parentHash)
	default:
		return dispatchContainerKind(ctx, r, kind, i, step, parentHash)
	}
}

// runAgentStep runs an agent step and registers what it decided, for any later
// step that declared context: { from: { <this step>: ... } }. The decision is
// recorded even when the run failed: a later visit of a revise loop reads the
// verdict that sent it back, and a failed step simply has no verdict.
func runAgentStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	out, err := agent.RunStep(ctx, r.cfg, r.jobName, i, step, r.bw, r.st, parentHash)

	recordStepOutcome(ctx, step, out)

	res := stepResult{hash: out.Hash, verdict: out.Verdict, note: out.Note}
	if err != nil {
		res.hash = ""

		return res, fmt.Errorf("agent step: %w", err)
	}

	if out.Cached {
		res.disposition = stepCacheHit
	}

	return res, nil
}

// dispatchContainerKind dispatches the block kinds — the ones that run other
// steps rather than doing work themselves. Split from dispatchByKind so
// neither switch has to carry both halves.
func dispatchContainerKind(
	ctx context.Context, r stepRunner, kind config.StepKind, i int, step config.Step, parentHash string,
) (stepResult, error) {
	switch kind { //nolint:exhaustive // the leaf kinds are handled by dispatchByKind; default catches config.StepKindGet, which never reaches here
	case config.StepKindTry:
		return runTryStep(ctx, r, i, step, parentHash)
	case config.StepKindInParallel:
		return runParallelStep(ctx, r, i, step, parentHash)
	case config.StepKindDo:
		return runDoStep(ctx, r, i, step, parentHash)
	case config.StepKindRace:
		return runRaceStep(ctx, r, i, step, parentHash)
	case config.StepKindEnsemble:
		return runEnsembleStep(ctx, r, i, step, parentHash)
	default: // config.StepKindGet — dispatchNonGetStep is only called for non-get steps
		return stepResult{}, unrecognizedStep(i)
	}
}

func unrecognizedStep(i int) error {
	return fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
}
