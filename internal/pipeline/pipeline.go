// Package pipeline orchestrates a job's plan: resolving/fetching get steps,
// running task/put/agent steps in order, and recording each step's outcome
// so later runs can skip unchanged work (see internal/merkle).
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// nodeRecord converts a plan merkle.Node into the shape store.RecordNode
// persists, keeping the store package free of a dependency on merkle's Node
// type.
func nodeRecord(n merkle.Node) store.NodeRecord {
	return store.NodeRecord{
		Hash:       n.Hash,
		ParentHash: n.ParentHash,
		Kind:       string(n.Kind),
		StepIndex:  n.StepIndex,
		Resource:   n.Resource,
		Content:    n.Content,
	}
}

// RunJob executes job's plan steps in order. pinned applies to any `get`
// step's version selection. provider materializes every build/step
// workspace the run needs — see workspace.Provider; when cfg.Workspace is
// nil, provider is the shared, single-mutable-directory implementation.
//
// Before executing anything, it statically validates every task/agent/put
// step's declared inputs (see workspace.ValidateArtifactFlow — always runs, even
// under --force) and plans every chain the job's steps could resolve to
// (resolving get versions but running nothing), checking the store for chains
// that already succeeded with identical content so that already-run
// get/task work can be skipped entirely. put steps are never skipped — see
// runSteps. skipCache (--force) bypasses only the chain-skip planning and
// re-runs everything, though results are still recorded as usual.
func RunJob(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, provider workspace.Provider, st *store.Store, skipCache bool) error {
	slog.Info("job.run", "job", job.Name, "steps", len(job.Plan))

	err := workspace.ValidateArtifactFlow(cfg, job)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	err = preflight(ctx, cfg, job)
	if err != nil {
		return err
	}

	if cfg.UsesImages() {
		err = shell.ValidateDocker(ctx)
		if err != nil {
			return fmt.Errorf("job %q: image: configured but docker is unavailable: %w", job.Name, err)
		}

		// Reclaim containers a previous run was SIGKILLed before it could
		// remove. Best-effort and silent when there is nothing to do, so this
		// costs one `docker ps` on the overwhelmingly common clean start.
		shell.SweepOrphanedContainers(ctx)

		// Pull now rather than letting the first step needing an uncached image
		// pay for it inside its own timeout, with the progress landing in that
		// command's output. Present images are a local inspect, so a warm run
		// (including every subsequent job under `steps watch`) costs nothing.
		err = shell.PrepareImages(ctx, cfg.Images())
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}
	}

	bw, err := provider.NewBuild(ctx, job.Name)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	// A run identifies itself so a failure can be continued rather than
	// restarted. Recorded before the first step, since a run that fails on
	// step one is still a run somebody may want to resume.
	resume := resumeFrom(ctx)
	if resume == nil {
		resume = &resumeState{id: NewRunID(), done: map[int]string{}}
		ctx = withResume(ctx, resume)
	}

	if rooted, ok := bw.(workspace.RootedBuild); ok {
		_ = st.StartRun(ctx, resume.id, job.Name, rooted.Root())
	}

	// Carry an execution log through this invocation so a job's assert.execution
	// can self-verify what ran (plan steps and hooks). The dispatch points and
	// runHookStep append to it; nothing outside pipeline touches it.
	log := &execLog{}
	ctx = withExecLog(ctx, log)

	// Identify this run so each agent's OpenRouter calls share a session and
	// keep its prompt cache warm across conversation turns, revise loops, and
	// repeated sub-agent calls (see agent.WithNewRun — the session is per
	// agent within the run, not run-wide). Scoped to this invocation so
	// concurrent jobs under `steps watch --max-concurrent` never share a
	// provider pin; ignored outright by every non-OpenRouter provider.
	ctx = agent.WithNewRun(ctx, job.Name)

	// --force means re-run everything, and everything includes the parts that
	// keep their own cache rather than consulting the chain index (across:
	// cells). Carried on the context because the runners that need it are
	// several frames below the flag.
	ctx = withForce(ctx, skipCache)

	// load_var: values are scoped to one job run: a var captured in one run
	// says nothing about the next.
	ctx = withRunVars(ctx)

	// Remember which versions this run fetched, so a successful job can mark
	// them green for any downstream job's passed: constraint.
	ctx, fetched := withFetchedVersions(ctx)

	// Account for what this job's agent steps spend, and enforce the job's
	// cumulative ceiling if it set one. Installed here, not per step, because
	// a job budget is by definition the sum across steps.
	usage := agent.NewRunUsage(jobBudgetTokens(job))
	ctx = agent.WithRunUsage(ctx, usage)

	// The same ceiling in the other unit. Installed alongside the budget for
	// the same reason: a wall-clock bound on a job is by definition a sum
	// across its steps, so it belongs to the run rather than to any one step.
	ctx = withJobDeadline(ctx, job)

	defer reportJobUsage(job.Name, usage)

	// Everything from here on has a workspace to run job-level hooks in, so
	// funnel planning and execution into one outcome and dispatch the job's
	// hooks around it. Pre-workspace failures above fire no hooks — there's
	// nowhere for a job-level hook to run yet. This is steps's own design
	// choice, not a Concourse parity claim: Concourse has no literal
	// job-level hook construct to compare against (its hooks are step
	// modifiers), so "matching Concourse" was unverifiable and has been
	// dropped from this comment — see docs/conformance.md. Job hooks fire on
	// every invocation past this point, cached or not; they are never hashed
	// or skipped.
	runErr := runJobPlan(ctx, cfg, job, pinned, provider, bw, st, skipCache)

	scope := hookScope{cfg: cfg, jobName: job.Name, label: fmt.Sprintf("job %q", job.Name), bw: bw}

	finalErr := runHooks(ctx, scope, job.Hooks, runErr)

	// A job assert is the final word: it runs after hooks so the log includes
	// them. A mismatch fails the job regardless, and is never itself cleared.
	finalErr = checkJobAssert(job, log, finalErr)
	if finalErr != nil {
		// Keep the workspace on failure rather than destroying it: the files a
		// step had just written when it failed are the most useful thing to
		// look at, and they are what a resume continues from.
		_ = st.FinishRun(context.WithoutCancel(ctx), resume.id, "failed")

		reportResumable(resume.id, bw)

		return finalErr
	}

	workspace.CloseBuild(bw, job.Name)

	_ = st.FinishRun(context.WithoutCancel(ctx), resume.id, "succeeded")

	recordPassedVersions(ctx, st, job.Name, fetched)

	slog.Info("job.done", "job", job.Name)

	return nil
}

// runJobPlan plans (unless skipCache) and runs a job's steps, returning the
// aggregate outcome that job-level hooks dispatch on. A planning failure
// classifies as errored (job on_error); a step failure carries whatever
// classification its producing site marked it with.
func runJobPlan(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, provider workspace.Provider, bw workspace.BuildWorkspace, st *store.Store, skipCache bool) error {
	skippable := map[string]bool{}

	// cache is scoped to this one RunJob invocation (never shared across
	// concurrent invocations — see resource.NewCache) and threaded into both
	// the plan-time and run-time get-step resolution below, so a get step's
	// check command runs at most once per job run instead of once during
	// planning and again during execution.
	cache := rsrc.NewCache()

	if !skipCache {
		chains, err := merkle.PlanChains(ctx, cfg, job.Name, job.Plan, pinned, cache)
		if err != nil {
			return fmt.Errorf("job %q: planning: %w", job.Name, err)
		}

		skippable, err = buildSkippableIndex(ctx, st, job.Name, chains)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}
	}

	return runSteps(ctx, cfg, job.Name, job.Plan, pinned, provider, bw, st, skippable, "", false, cache, true)
}

// computeChainSkippable reports, per chain, whether it's already covered by a
// prior succeeded job_runs row — batched into one query (per
// Store.HasSucceededBatch) instead of one per chain. An Unskippable chain is
// never even asked about; it stays false.
func computeChainSkippable(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) ([]bool, error) {
	chainSkippable := make([]bool, len(chains))

	toCheck := make([]string, 0, len(chains))

	for _, chain := range chains {
		if !chain.Unskippable {
			toCheck = append(toCheck, chain.RootHash)
		}
	}

	succeeded, err := st.HasSucceededBatch(ctx, jobName, toCheck)
	if err != nil {
		return nil, fmt.Errorf("has succeeded batch: %w", err)
	}

	for i, chain := range chains {
		if !chain.Unskippable {
			chainSkippable[i] = succeeded[chain.RootHash]
		}
	}

	return chainSkippable, nil
}

// buildSkippableIndex returns, for every node hash reachable across chains,
// whether every leaf merkle.Chain passing through it is already covered by a
// prior succeeded job_runs row. Any Unskippable chain (contains a put or
// agent step) is forced non-skippable everywhere along it — those steps
// (and everything feeding them) must always run. A node hash shared by
// multiple chains is skippable only if ALL chains through it are skippable
// (AND-rollup), which correctly forces get/task ancestors of an
// unskippable branch to execute even if a sibling branch is independently
// skippable.
func buildSkippableIndex(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) (map[string]bool, error) {
	chainSkippable, err := computeChainSkippable(ctx, st, jobName, chains)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", jobName, err)
	}

	nodeChains := map[string][]int{}

	for i, chain := range chains {
		for _, node := range chain.Nodes {
			nodeChains[node.Hash] = append(nodeChains[node.Hash], i)
		}
	}

	skippable := make(map[string]bool, len(nodeChains))

	for hash, idxs := range nodeChains {
		all := true

		for _, idx := range idxs {
			if !chainSkippable[idx] {
				all = false

				break
			}
		}

		skippable[hash] = all
	}

	return skippable, nil
}

// stepDisposition is what happened to one non-get step, distinguishing the
// two very different reasons a step might not have executed.
type stepDisposition int

const (
	// stepRan: the step executed; advance parentHash to its node.
	stepRan stepDisposition = iota
	// stepGuardSkipped: the step's when: guard was false. Only THIS step is
	// skipped — the plan continues with the next one, and parentHash does not
	// advance (no node was produced).
	stepGuardSkipped
	// stepChainSkipped: the step's hash matched an already-succeeded chain, so
	// everything downstream of it also already succeeded. The whole remaining
	// plan is skipped.
	stepChainSkipped
)

// nonGetOutcome is what dispatchNonGetStep/runNonGetStep report about a
// step's routing-relevant outcome, beyond its hash/disposition/error: the
// verdict applyRouting keys on, and — for an agent step only — the verdict's
// note and this step's own run (see agent.PreviousRun), both of which
// runSteps threads into a routed-to successor's Handoff. Its zero value is
// exactly right for task/put steps and any non-stepRan disposition.
type nonGetOutcome struct {
	verdict  string
	note     string
	previous *agent.PreviousRun
}

// runSteps executes steps in order. A `get` step fans out: for each version
// it selects (a single version normally, or every version returned by
// check when version:every is set), that version triggers its own build
// of the remainder of the plan — see runTriggeredBuild. It always
// terminates this loop, since it delegates the rest of the plan to its
// triggered build(s). A `task`/`put`/`agent` step is handled by
// runNonGetStep; `put`/`agent` steps are never looked up in skippable and
// always execute.
func runSteps(
	ctx context.Context, cfg *config.Config, jobName string, steps []config.Step, pinned map[string]string,
	provider workspace.Provider, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool,
	parentHash string, chainUnskippable bool, cache *rsrc.Cache,
	allowGetTrigger bool,
) error {
	// visits counts how many times each step index has executed this
	// invocation, bounding a to:-driven backward loop. It's per-runSteps-call,
	// so each triggered build (and each version under get: version: every, which
	// re-enters via a fresh runSteps) gets its own independent max_visits budget.
	visits := map[int]int{}

	// pending is the Handoff a routed transition builds for whichever step
	// index it targets — consumed (and cleared) the moment that step is next
	// dispatched, whether or not its handoff: actually uses it. nil means "no
	// pending transition into the next step" (a straight fall-through, or the
	// very first step), which is the overwhelmingly common case and costs
	// nothing extra.
	var pending *agent.Handoff

	// A manual index loop (not range) so a to: transition can set the next
	// index — forward to skip ahead, or backward to loop. Without any to:, the
	// default nextIndex of i+1 reproduces today's straight-line behavior exactly.
	for i := 0; i < len(steps); {
		// Between steps, never during one: the step that was running has
		// finished and kept its work, and this decides only whether another
		// starts. See deadline.go.
		err := jobDeadlinePassed(ctx, jobName)
		if err != nil {
			return err
		}

		step := steps[i]

		if step.Get != "" && !allowGetTrigger {
			done, err := runGetStepInPlaceResult(ctx, cfg, jobName, i, step, steps, pinned, bw, st, skippable, &parentHash, cache)
			if done {
				return err
			}
			i++
			continue
		}
		if step.Get != "" {
			return runGetStep(ctx, cfg, jobName, i, step, steps[i+1:], pinned, provider, st, skippable, parentHash, chainUnskippable, cache)
		}

		returned, err := executeNonGetStep(ctx, cfg, jobName, &i, &parentHash, &chainUnskippable, &pending, step, steps, bw, st, skippable, visits)
		if returned {
			return err
		}
	}

	// Once more after the last step. The check above runs BEFORE a step, so a
	// deadline that passed during the final one — or during a fan-out that
	// stopped admitting cells because of it — would otherwise never be looked
	// at again, and the job would report success having overrun.
	err := jobDeadlinePassed(ctx, jobName)
	if err != nil {
		return err
	}

	return recordChainSucceeded(ctx, st, jobName, parentHash, chainUnskippable)
}

// executeNonGetStep runs one iteration's non-get (task/put/agent) step through
// execution, routing, and chain-unskippable folding, mutating its pointer
// arguments so runSteps can continue the loop or return. It returns
// (true, err) to return from runSteps (nil err = chain-skip),
// (false, nil) to advance i and continue the loop.
func executeNonGetStep(
	ctx context.Context,
	cfg *config.Config,
	jobName string,
	i *int,
	parentHash *string,
	chainUnskippable *bool,
	pending **agent.Handoff,
	step config.Step,
	steps []config.Step,
	bw workspace.BuildWorkspace,
	st *store.Store,
	skippable map[string]bool,
	visits map[int]int,
) (bool, error) {
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
	if skipCompletedStep(ctx, jobName, i) {
		return false, nil
	}

	newParentHash, disposition, no, err := runNonGetStep(ctx, cfg, jobName, *i, step, bw, st, skippable, *parentHash, handoffFor(step, *pending))

	if disposition == stepRan {
		visits[*i]++
	}

	recordCompletedStep(ctx, st, *i, step, err)

	nextIndex, routedKey, err, exhaustedErr := applyRouting(ctx, steps, *i, step, disposition, no.verdict, err, visits)
	if exhaustedErr != nil {
		return true, exhaustedErr
	}

	// Last, so a try: wrapper's to: has already routed on the real outcome and
	// the wrapper's hooks have already observed it.
	err = tolerateTryFailure(ctx, jobName, step, err)

	if err != nil {
		return true, err
	}

	if disposition == stepChainSkipped {
		reportChainSkipped(jobName, steps[*i+1:])

		return true, nil
	}

	var foldErr error
	*chainUnskippable, foldErr = foldStepUnskippable(cfg, step, *chainUnskippable)
	if foldErr != nil {
		return true, foldErr
	}

	if disposition == stepGuardSkipped {
		*pending = nil
		*i = nextIndex

		return false, nil
	}

	if newParentHash != "" {
		*parentHash = newParentHash
	}

	*pending = nextPendingHandoff(jobName, step, steps, routedKey, no, visits, nextIndex)
	*i = nextIndex

	return false, nil
}

// reportChainSkipped prints one "skip: <name> (chain)" line per step after a
// chain-skip point, matching the "skip: <name> (when)"/"skip: <name>
// (version: ...)" convention used elsewhere, so a cached rerun's transcript
// still names every downstream step instead of going silent after the
// triggering task's own "skip:" line. It only names steps from their config
// — it never resolves a get step's version or does any of the work runSteps
// is skipping, so it can't reintroduce the resource checks caching exists to
// avoid.
func reportChainSkipped(jobName string, steps []config.Step) {
	for _, step := range steps {
		name := executedStepName(step)
		if name == "" {
			name = step.Get // executedStepName covers task/put/agent only
		}

		if name == "" {
			continue
		}

		fmt.Printf("skip: %s (chain)\n", name)
		slog.Info("job.skip", "job", jobName, "step", name, "reason", "chain")
	}
}

// handoffFor returns pending when step's own handoff: enables something —
// the step is what actually consumes carried transition context — and nil
// otherwise, so a step without handoff: never sees a pending value even when
// one exists. Split out of runSteps to keep its cyclomatic complexity down.
func handoffFor(step config.Step, pending *agent.Handoff) *agent.Handoff {
	// Unwrap: handoff: sits on the agent step, which a try: wrapper hides —
	// the wrapper itself never carries one (load-time rejected as "handoff is
	// only valid on agent steps"), so without this a tolerated agent was
	// reached with a nil Handoff and answered a redo as if freshly started.
	inner := step.Unwrap()
	if inner.Handoff != nil && inner.Handoff.Enabled() {
		return pending
	}

	return nil
}

// nextPendingHandoff builds the Handoff a just-routed step hands to whichever
// step index it targeted, or nil when the step didn't route (routedKey ==
// ""). Split out of runSteps as a pure function so the carry's construction —
// which fields come from where — is unit-testable without a live agent.
// visits[nextIndex] is read BEFORE runSteps' next iteration would increment
// it, so Visit correctly previews "this will be the Nth execution of
// nextIndex".
func nextPendingHandoff(jobName string, step config.Step, steps []config.Step, routedKey string, no nonGetOutcome, visits map[int]int, nextIndex int) *agent.Handoff {
	if routedKey == "" {
		return nil
	}

	// `to: <key>: next` on the LAST step of a plan slice routes one past the
	// end — the same place an unrouted final step goes. There is no step there
	// to hand anything to, and the field reads below would index out of range:
	// a real panic, on exactly the pipeline whose last outcome says "carry on".
	if nextIndex >= len(steps) {
		return nil
	}

	return &agent.Handoff{
		JobName:   jobName,
		FromStep:  executedStepName(step),
		RouteKey:  routedKey,
		Note:      no.note,
		Visit:     visits[nextIndex] + 1,
		MaxVisits: steps[nextIndex].MaxVisits,
		StepIndex: nextIndex,
		PlanLen:   len(steps),
		Previous:  no.previous,
	}
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

// skipCompletedStep skips a plan step a previous attempt of this run already
// finished, advancing the index. It reports whether it skipped.
func skipCompletedStep(ctx context.Context, jobName string, i *int) bool {
	name, done := resumeFrom(ctx).alreadyDone(*i)
	if !done {
		return false
	}

	fmt.Printf("skip: %s (already succeeded)\n", name)
	slog.Info("job.skip", "job", jobName, "index", *i, "reason", "resume", "step", name)
	recordExecution(ctx, name)

	*i++

	return true
}

// runNonGetStep runs a task/put/agent step and dispatches its hooks around the
// outcome. A skipped (merkle-cached) step fires no hooks — it did not run, so
// there is nothing for its observers to react to. When a green step is failed
// by its own on_success/ensure hook, the true (failed) outcome is recorded so
// the store and skip-cache reflect it.
func runNonGetStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string, handoff *agent.Handoff) (string, stepDisposition, nonGetOutcome, error) {
	hash, disposition, no, err := dispatchNonGetStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash, handoff)

	// Record what ran (not a cached chain, not a guard-skipped step) for a
	// job's assert.execution, before hooks so the order reads
	// [step, its hooks...].
	if disposition == stepRan {
		recordStepExecution(ctx, step)
	}

	// Neither kind of skip fires hooks: the step did not run, so it has no
	// outcome for its observers to react to.
	if disposition != stepRan || step.Hooks.Empty() {
		return hash, disposition, no, err
	}

	scope := hookScope{cfg: cfg, jobName: jobName, label: stepLabel(i, step), bw: bw}

	final := runHooks(ctx, scope, step.Hooks, err)
	if err == nil && final != nil {
		recCtx := context.WithoutCancel(ctx)
		_ = st.RecordJobRun(recCtx, jobName, hash, string(outcome.Failed), final)

		return "", stepRan, no, final
	}

	return hash, disposition, no, final
}

// dispatchNonGetStep dispatches a task/put/agent step — the three kinds that,
// unlike get, run in place and return a single new parentHash rather than
// fanning out or delegating the remainder of the plan. It first evaluates the
// step's when: guard (see evaluateStepGuard): a false guard skips only this
// step. stepChainSkipped is only ever returned for a cache-matched task step;
// put/agent steps are never chain-skippable. handoff is threaded straight
// into agent.RunStep — see runSteps' pending carry — and ignored by task/put.
func dispatchNonGetStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string, handoff *agent.Handoff) (string, stepDisposition, nonGetOutcome, error) {
	shouldRun, err := evaluateStepGuard(ctx, cfg, step, bw)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (when): %w", i, err)
	}

	if !shouldRun {
		fmt.Printf("skip: %s (when)\n", executedStepName(step))
		slog.Info("job.skip", "job", jobName, "index", i, "reason", "when", "step", executedStepName(step))

		return parentHash, stepGuardSkipped, nonGetOutcome{}, nil
	}

	// A captured load_var: value changes what a step runs, so substitute
	// before anything hashes or executes it.
	step = renderStepVars(ctx, step)

	if step.LoadVar != "" {
		return runLoadVarStep(ctx, jobName, i, step, bw, st, parentHash)
	}

	if step.Approval != nil {
		return runApprovalStep(ctx, jobName, i, step, st, parentHash)
	}

	// across: is a MODIFIER, not a kind: the step it sits on is still a task
	// (or a put, or an agent), it just runs once per combination. Checking it
	// before resolving the kind is what keeps it off Step.Kind()'s table,
	// where it would read as a second kind on every step that uses it.
	if len(step.Across) > 0 {
		return runAcrossStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
	}

	kind, ok := step.Kind()
	if !ok {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}

	return dispatchByKind(ctx, cfg, jobName, i, kind, step, bw, st, skippable, parentHash, handoff)
}

// dispatchByKind routes a resolved non-get step to its runner. Split from the
// guard evaluation above purely to stay inside the complexity budget; the
// kinds it handles are the whole non-get set, which `exhaustive` enforces.
func dispatchByKind(
	ctx context.Context, cfg *config.Config, jobName string, i int, kind config.StepKind, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	switch kind { //nolint:exhaustive // default covers config.StepKindGet — this is only called for non-get steps
	case config.StepKindTask:
		hash, disposition, err := runTaskStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash)

		return hash, disposition, nonGetOutcome{}, err
	case config.StepKindPut:
		hash, err := runPutStep(ctx, cfg, jobName, i, step, bw, st, parentHash)

		return hash, stepRan, nonGetOutcome{}, err
	case config.StepKindAgent:
		stepOut, err := agent.RunStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
		no := nonGetOutcome{verdict: stepOut.Verdict, note: stepOut.Note, previous: stepOut.Previous}

		if err != nil {
			return "", stepRan, no, fmt.Errorf("agent step: %w", err)
		}

		return stepOut.Hash, stepRan, no, nil
	case config.StepKindTry:
		return runTryStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
	case config.StepKindInParallel:
		return runParallelStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
	case config.StepKindRace:
		return runRaceStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
	case config.StepKindEnsemble:
		return runEnsembleStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
	default: // config.StepKindGet — dispatchNonGetStep is only called for non-get steps
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// recordChainSucceeded records the leaf of a fully-executed chain as
// succeeded, unless it contains a put or agent step (those chains are
// never skippable, so recording job_runs for them would be unused).
func recordChainSucceeded(ctx context.Context, st *store.Store, jobName, rootHash string, chainUnskippable bool) error {
	if chainUnskippable {
		return nil
	}

	err := st.RecordJobRun(ctx, jobName, rootHash, "succeeded", nil)
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	return nil
}

// runTryStep executes a try: wrapper. It runs the inner step exactly as if it
// were unwrapped — through runNonGetStep, so the inner step's own when: guard,
// hooks and execution log all behave normally — and hands its REAL outcome
// (disposition, verdict, error) back to the plan walker.
//
// Nothing is swallowed here. Toleration is deliberately the last thing that
// happens to the error, in executeNonGetStep, after routing: that is what lets
// a `to: {failure: ...}` on the wrapper fire, and what keeps an aborted or
// infrastructure-errored inner step from being reported as a green job. An
// earlier revision called dispatchNonGetStep and returned nil from here, which
// cost all three at once.
func runTryStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff) (string, stepDisposition, nonGetOutcome, error) {
	content, err := merkle.TryNodeContent(cfg, step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (try): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindTry, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (try): %w", i, err)
	}

	inner := *step.Try
	name := executedStepName(inner)

	fmt.Printf("try: %s\n", name)
	slog.Debug("job.step", "job", jobName, "index", i, "kind", "try", "inner", name)

	// Run the inner step with the try node's hash as parent — the inner step
	// chains under the try wrapper. No caching (nil skippable): try is always
	// unskippable, so the inner step always runs.
	_, disposition, innerNo, innerErr := runNonGetStep(ctx, cfg, jobName, i, inner, bw, st, nil, hash, handoff)

	// The wrapper's node status is what the plan did with the outcome, not
	// what the inner step's outcome was (the inner step records that itself):
	// "succeeded" when the failure is about to be tolerated, "failed" when it
	// is one of the classes try: does not cover and the job stops here.
	status := "succeeded"
	if innerErr != nil && !toleratedByTry(ctx, innerErr) {
		status = "failed"
	}

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindTry, StepIndex: i, Resource: name, Content: content}
	recCtx := context.WithoutCancel(ctx)
	_ = st.RecordNode(recCtx, nodeRecord(node), jobName, status, nil, innerErr)

	return hash, disposition, innerNo, innerErr
}

// toleratedByTry reports whether err is the kind of outcome a try: wrapper
// exists to swallow: a task-level failure and nothing else.
//
// An Errored (infrastructure) or Aborted (ctx-canceled) step is NOT tolerated.
// This is the same line outcomeKey draws for to: routing, for the same reason:
// swallowing them would report a green job for a Ctrl-C or a docker outage,
// and would let the plan march on into steps whose context is already dead.
func toleratedByTry(ctx context.Context, err error) bool {
	return err != nil && outcome.Classify(ctx, err) == outcome.Failed
}

// tolerateTryFailure is a try: wrapper's whole effect on the plan: it turns the
// inner step's task-level failure into a nil error so runSteps continues, and
// says so on the transcript. It runs AFTER applyRouting, so a wrapper that
// routed on the failure has already consumed the error and prints nothing extra
// here. Any non-try step, and any outcome try: doesn't cover, passes through.
func tolerateTryFailure(ctx context.Context, jobName string, step config.Step, err error) error {
	if step.Try == nil || !toleratedByTry(ctx, err) {
		return err
	}

	name := executedStepName(step)

	fmt.Printf("try: %s failed (tried, continuing)\n", name)
	slog.Info("job.try", "job", jobName, "step", name, "outcome", "tolerated", "error", err.Error())

	return nil
}

// runGetStep resolves and (unless skippable) fetches step's resource
// version(s), then runs the remainder of the plan for each — see
// runTriggeredBuild. It always terminates the calling runSteps loop, since
// a get step delegates the rest of the plan to its triggered build(s).
//
// A version whose triggered build fails does NOT stop the remaining
// versions from being attempted (see TestConformanceGetVersionEveryContinuesPastFailure
// in conformance_test.go): Concourse's own version-selection cursor
// (atc/db/versions_db.go's NextEveryVersion) advances regardless of a prior
// build's status, and every version here already gets its own isolated
// workspace/hooks/store-recording via runTriggeredBuild — nothing about
// stopping early was ever load-bearing for correctness, only an accident of
// the loop giving up on the first error. Structural errors (bad template,
// unmarshalable version — GetNodeContent/HashNode) are the one exception:
// those depend only on static step/version content, so they'll recur
// identically for every version, and aborting immediately is still right.
func runGetStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, remainder []config.Step,
	pinned map[string]string, provider workspace.Provider, st *store.Store, skippable map[string]bool,
	parentHash string, chainUnskippable bool, cache *rsrc.Cache,
) error {
	resource, resourceType, versions, err := fetchGetVersions(ctx, cfg, step, pinned, cache)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "get", "resource", step.Get, "versions", len(versions))

	// version:every is the ONLY path that can reach here with nothing: every
	// other mode narrows to a pin, and SelectVersion errors on an empty check
	// (internal/resource/resource.go). So this loop runs zero builds, returns
	// nil, and the job "succeeds" — outwardly identical to one whose steps all
	// ran. The two cases want opposite reactions ("nothing new upstream" is
	// idle; "the check is broken or its source is gone" is not), so say which
	// resource went empty and how much of the plan that silently dropped.
	if len(versions) == 0 {
		fmt.Printf("get: %s returned no versions; the %d step(s) after it did not run\n", resource.Name, len(remainder))
		slog.Warn("job.get.no_versions", "job", jobName, "index", i, "resource", resource.Name, "skipped_steps", len(remainder))
	}

	var buildErrs []error

	for _, version := range versions {
		// Concourse's scheduler has no equivalent of "the whole CLI process is
		// shutting down" mid-cursor-advance to compare against — this is
		// steps's own judgment, not a Concourse claim. It mirrors the
		// ctx.Err() check in internal/trigger/trigger.go's worker loop: stop
		// starting NEW triggered builds on cancellation, don't let one abandon
		// itself mid-flight.
		if ctx.Err() != nil {
			break
		}

		content, err := merkle.GetNodeContent(cfg, step, *resourceType, resource.Source, version)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		hash, err := merkle.HashNode(merkle.NodeKindGet, content, parentHash)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		if skippable[hash] {
			fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
			slog.Info("job.skip", "job", jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)

			continue
		}

		node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		err = runTriggeredBuild(ctx, cfg, jobName, i, step, *resource, *resourceType, version, remainder, pinned, provider, st, skippable, node, chainUnskippable, cache)
		if err != nil {
			buildErrs = append(buildErrs, fmt.Errorf("step %d (get %q) version %v: %w", i, step.Get, version, err))

			continue
		}
	}

	return errors.Join(buildErrs...)
}

// runGetStepInPlace fetches one version of a get step's resource into an
// existing build workspace (bw) rather than creating a new triggered build.
// It returns the new parentHash on success, or stepChainSkipped when the
// node's hash is already in the skippable index. Only called from runSteps
// when allowGetTrigger is false — i.e., inside a triggered build's remainder,
// where consecutive gets share the same workspace.
func runGetStepInPlace(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	pinned map[string]string, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool,
	parentHash string, cache *rsrc.Cache,
) (string, stepDisposition, error) {
	resource, resourceType, versions, err := fetchGetVersions(ctx, cfg, step, pinned, cache)
	if err != nil {
		return "", stepRan, err
	}

	// Same silence, milder consequence: the rest of the plan still runs, just
	// without the artifact this get was supposed to materialize, so a later
	// step fails on a missing input instead of on the empty check that caused
	// it. Name the cause here, where it is known.
	if len(versions) == 0 {
		fmt.Printf("get: %s returned no versions; nothing was fetched\n", step.Get)
		slog.Warn("job.get.no_versions", "job", jobName, "index", i, "resource", step.Get)

		return parentHash, stepRan, nil
	}

	// Inside a triggered build a get resolves to a single version.
	version := versions[0]

	recordFetchedVersion(ctx, resource.Name, version)

	content, err := merkle.GetNodeContent(cfg, step, *resourceType, resource.Source, version)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindGet, content, parentHash)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	if skippable[hash] {
		fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
		slog.Info("job.skip", "job", jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)

		return parentHash, stepChainSkipped, nil
	}

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

	err = fetchGetStepWithStep(ctx, cfg, step, step.Get, *resource, *resourceType, version, bw)
	if err != nil {
		recordStepFailure(ctx, st, node, jobName, err)

		return "", stepRan, err
	}

	// Get-step hooks fire in the same workspace the resource was fetched into.
	if !step.Hooks.Empty() {
		scope := hookScope{cfg: cfg, jobName: jobName, label: stepLabel(i, step), bw: bw}
		err = runHooks(ctx, scope, step.Hooks, err)
		if err != nil {
			recordStepFailure(ctx, st, node, jobName, err)

			return "", stepRan, err
		}
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", nil, nil)
	if err != nil {
		return "", stepRan, fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return hash, stepRan, nil
}

// runGetStepInPlaceResult calls runGetStepInPlace and translates its result
// into a "done/error" pair runSteps acts on: (done=true, err=nil) means
// "the chain was skipped — return nil"; (done=true, err!=nil) means
// "something failed — return err"; (done=false, err=nil) means
// "fetch succeeded — advance i and continue the loop".
func runGetStepInPlaceResult(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, steps []config.Step,
	pinned map[string]string, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool,
	parentHash *string, cache *rsrc.Cache,
) (bool, error) {
	newParentHash, disposition, err := runGetStepInPlace(ctx, cfg, jobName, i, step, pinned, bw, st, skippable, *parentHash, cache)
	if err != nil {
		return true, err
	}
	if disposition == stepChainSkipped {
		reportChainSkipped(jobName, steps[i+1:])
		return true, nil
	}
	if newParentHash != "" {
		*parentHash = newParentHash
	}
	return false, nil
}

// runTaskStep hashes step against parentHash and, unless that hash is
// skippable, runs it. It returns the hash to use as parentHash for the
// next step (unchanged, along with skipped=true, when skipped).
func runTaskStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string) (string, stepDisposition, error) {
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d: %w", i, err)
	}

	content, err := merkle.TaskNodeContent(cfg, step, rt)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindTask, content, parentHash)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	if skippable[hash] {
		// Every other skip line names its reason — (chain), (when),
		// (version: ...) — and this one, the cache hit that triggers all the
		// (chain) lines below it, was the only bare one.
		fmt.Printf("skip: %s (cached)\n", rt.Name)
		slog.Info("job.skip", "job", jobName, "index", i, "kind", "task", "task", rt.Name, "reason", "cached", "hash", hash)

		// The command did not run, so anything it recorded has to come back
		// from what it recorded last time — otherwise a cached run reaches the
		// agent steps with facts a fresh run would have had.
		err = replayTaskContext(ctx, st, agent.ContextWriteScope(ctx), rt.Name, hash)
		if err != nil {
			return "", stepChainSkipped, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		}

		return parentHash, stepChainSkipped, nil
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "task", "task", rt.Name, "run", rt.Run)

	// The name the step is KNOWN by, which for an across: cell is its labelled
	// identity rather than the task it resolves through — so the run line, the
	// recorded node, and the skip line on the next run all say the same thing.
	name := executedStepName(step)

	fmt.Printf("task: %s\n", name)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindTask, StepIndex: i, Resource: name, Content: content}

	collected, err := executeTask(ctx, cfg, step, rt, bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		recordStepFailure(ctx, st, node, jobName, wrapped)

		return "", stepRan, wrapped
	}

	// Recorded on the node as well as in the run context, so a later skip of
	// this same step can replay it (see replayTaskContext).
	err = recordTaskContext(ctx, st, agent.ContextWriteScope(ctx), rt.Name, collected)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", taskNodeResult(collected), nil)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	return hash, stepRan, nil
}

// retryWithTimeout runs fn up to attempts times (attempts < 1 is treated as
// 1), giving each attempt its own context bounded by timeoutStr when that
// parses to a positive duration — per-attempt, not a single deadline shared
// across the retries. On a retry (the second attempt onward) it calls marker
// with the 1-based attempt number and the total so the caller can print its
// own progress line. It is the single retry+per-attempt-timeout scaffold every
// get/task/put step shares; a per-attempt timeout expires only the attempt's
// context, leaving the parent ctx (which governs retry.Do's backoff and abort)
// untouched, so a job abort stays distinguishable from a step overrunning its
// own budget. An overrun ends the step immediately (retry.Stop) rather than
// spending the remaining attempts re-failing against the same budget.
func retryWithTimeout(ctx context.Context, attempts int, timeoutStr string, marker func(attempt, total int), fn func(ctx context.Context) error) error {
	timeout, err := config.ParseTimeout(timeoutStr)
	if err != nil {
		return err //nolint:wrapcheck // caller wraps with its own step context
	}

	total := attempts
	if total < 1 {
		total = 1
	}

	return retry.Do(ctx, total, func(attempt int) error { //nolint:wrapcheck // every caller wraps this func's own return with its step context
		if attempt > 0 && marker != nil {
			marker(attempt+1, total)
		}

		attemptCtx := ctx
		if timeout > 0 {
			var cancel context.CancelFunc
			attemptCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		// On this step's own wall clock expiring, stop: the same work against
		// the same budget would just expire again.
		return retry.StopOnDeadline(ctx, attemptCtx, fn(attemptCtx))
	})
}

// executeTask materializes a task's (isolated or shared) working directory,
// runs its command with retries and timeout, and captures its declared outputs
// — with no merkle/store recording. Shared by runTaskStep (which records the
// aggregate outcome) and hook execution (where the enclosing step/job records it).
// A `context: write` task's recorded facts are collected here, before the
// space closes — the values go to SQLite rather than to another step's
// directory, so unlike a handoff note this works under every workspace
// strategy.
func executeTask(ctx context.Context, cfg *config.Config, step config.Step, rt config.ResolvedTask, bw workspace.BuildWorkspace) (map[string]string, error) {
	space, err := bw.TaskSpace(ctx, rt.Name, rt.Inputs, rt.Outputs, rt.InputMapping, rt.OutputMapping)
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", rt.Name, err)
	}
	defer workspace.CloseSpace(space, rt.Name)

	err = retryWithTimeout(ctx, step.Attempts, rt.Timeout, func(attempt, total int) {
		fmt.Printf("task: %s (attempt %d/%d)\n", executedStepName(step), attempt, total)
		slog.Info("job.task.attempt", "task", executedStepName(step), "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		return runTaskCommand(attemptCtx, cfg, rt, space.Dir())
	})
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", rt.Name, err)
	}

	err = space.Capture(ctx)
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", rt.Name, err)
	}

	if !step.WritesContext() {
		return map[string]string{}, nil
	}

	collected, err := collectTaskContext(space.Dir())
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", rt.Name, err)
	}

	return collected, nil
}

// recordStepFailure records a step's failed node and job_run, classifying the
// outcome (failed vs errored vs aborted) and writing under a detached context
// so an aborted step's outcome still persists rather than being dropped by the
// canceled context. Best-effort: recording errors are ignored so they can't
// mask the original error returned to the caller.
func recordStepFailure(ctx context.Context, st *store.Store, node merkle.Node, jobName string, err error) {
	status := string(outcome.Classify(ctx, err))
	recCtx := context.WithoutCancel(ctx)
	_ = st.RecordNode(recCtx, nodeRecord(node), jobName, status, nil, err)
	_ = st.RecordJobRun(recCtx, jobName, node.Hash, status, err)
}

// runTaskCommand runs a task's run: command. Without a fix:, it streams
// output live and any nonzero exit is a hard failure (unchanged behavior).
// With a fix:, it captures output instead, and on a nonzero exit invokes the
// fix agent — seeded with that output and given the task itself as a rerun
// tool — then re-runs the command once; that re-run's exit code is the
// verdict. A green run never constructs the agent.
func runTaskCommand(ctx context.Context, cfg *config.Config, rt config.ResolvedTask, workspaceDir string) error {
	runner, err := shell.NewRunner(shell.RunnerSpec{Image: rt.Image, Cwd: workspaceDir, Env: rt.Env, User: rt.User, Network: rt.Network})
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name)
	defer shell.CloseRunner(runner, rt.Name)

	switch {
	case rt.Assert != nil:
		return runAssertedTask(ctx, runner, rt)
	case rt.Fix != nil:
		return runFixTask(ctx, cfg, runner, rt, workspaceDir)
	default:
		err := runner.Run(ctx, rt.Run)
		if err != nil {
			// Check for context cancellation/timeout first, before classifying
			// as a task failure. A canceled context (job abort) or timeout should
			// propagate as-is rather than being wrapped as a task failure.
			cancelErr := shell.CanceledError(ctx)
			if cancelErr != nil {
				return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
			}

			if shell.IsExitError(err) {
				err = outcome.Fail(err)
			}

			return fmt.Errorf("task %q: %w", rt.Name, err)
		}

		return nil
	}
}

// runFixTask runs a task with a fix: agent: capture the output, and on a
// nonzero exit invoke the fix agent (seeded with that output and given the
// task itself as a rerun tool), then re-run the command once — that re-run's
// exit code is the verdict. A green first run never constructs the agent.
func runFixTask(ctx context.Context, cfg *config.Config, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
	stdout, stderr, exitCode, err := runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	// RunCaptureFull reports even a signal-killed process (e.g. from a
	// canceled ctx) as data, not err — checked before treating exitCode as a
	// genuine verdict, so a run interrupted by shutdown isn't misread as a
	// real failure worth invoking the fix agent over.
	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	printTaskOutput(rt.Name, stdout, stderr)

	if exitCode == 0 {
		return nil
	}

	fmt.Printf("task %q failed (exit %d); invoking fix agent %q\n", rt.Name, exitCode, rt.Fix.Agent)

	err = agent.RunFix(ctx, cfg, rt, taskFailureOutput(stdout, stderr, exitCode), workspaceDir)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", rt.Fix.Agent, err)
	}

	// Verdict: re-run the command (its run:, not its fix:) and gate on it.
	stdout, stderr, exitCode, err = runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	cancelErr = shell.CanceledError(ctx)
	if cancelErr != nil {
		return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	printTaskOutput(rt.Name, stdout, stderr)

	if exitCode != 0 {
		return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(fmt.Errorf("still failing after fix agent %q (exit %d)", rt.Fix.Agent, exitCode)))
	}

	return nil
}

// runAssertedTask runs rt.Run capturing its output, then evaluates rt.Assert:
// a matching stdout substring and exit code make the task a success even on a
// non-zero exit; a mismatch is a task-level failure with a got-vs-want reason.
// assert takes over the success determination, so a task's fix: is not
// consulted when an assert is present.
func runAssertedTask(ctx context.Context, runner shell.Runner, rt config.ResolvedTask) error {
	stdout, stderr, exitCode, err := runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	// RunCaptureFull reports even a signal-killed process (e.g. from a
	// canceled ctx) as data, not err — checked before evaluating the assert,
	// so a run interrupted by shutdown isn't misread as a genuine mismatch.
	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	printTaskOutput(rt.Name, stdout, stderr)

	mismatch := assertMismatch(rt.Assert, stdout, exitCode)
	if mismatch != nil {
		return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(mismatch))
	}

	return nil
}

// assertMismatch returns a reason when captured stdout/exit code don't satisfy
// assert, or nil when they match. Code is exact; Stdout is a substring test.
func assertMismatch(assert *config.Assert, stdout string, exitCode int) error {
	if assert.Code != nil && *assert.Code != exitCode {
		return fmt.Errorf("assert.code: want %d, got %d", *assert.Code, exitCode)
	}

	if assert.Stdout != nil && !strings.Contains(stdout, *assert.Stdout) {
		return fmt.Errorf("assert.stdout: output does not contain %q", *assert.Stdout)
	}

	return nil
}

// printTaskOutput echoes a captured task run's streams to the terminal,
// prefixed with label (rt.Name), so a fix/assert-enabled task's output is
// still visible (RunCaptureFull buffers rather than streaming live the way
// Run does) and attributable to the task that produced it.
func printTaskOutput(label, stdout, stderr string) {
	if stdout != "" {
		fmt.Print(shell.PrefixLines(label, stdout))
	}

	if stderr != "" {
		fmt.Fprint(os.Stderr, shell.PrefixLines(label, stderr))
	}
}

// fetchGetVersions resolves a get step's versions with retries and timeout support.
// It returns the resource, resource type, and versions to fetch.
func fetchGetVersions(ctx context.Context, cfg *config.Config, step config.Step, pinned map[string]string, cache *rsrc.Cache) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	var (
		resource     *config.Resource
		resourceType *config.ResourceType
		versions     []map[string]any
	)

	err := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		fmt.Printf("get: %s (attempt %d/%d)\n", step.Get, attempt, total)
		slog.Info("job.get.attempt", "get", step.Get, "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		res, resType, vers, fetchErr := cache.ResolveVersionsCached(attemptCtx, cfg, step, pinned)
		if fetchErr != nil {
			return fetchErr //nolint:wrapcheck // wrapped with get context by the caller below
		}

		resource, resourceType, versions = res, resType, vers

		return nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	return resource, resourceType, versions, nil
}

// taskFailureOutput formats a failed run's exit code and streams into the
// text seeded into the fix agent's prompt.
func taskFailureOutput(stdout, stderr string, exitCode int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "exit code: %d\n", exitCode)

	if stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(stdout)
		b.WriteString("\n")
	}

	if stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(stderr)
		b.WriteString("\n")
	}

	return b.String()
}

// runPutStep hashes and always runs step (put steps are never skipped),
// returning the hash to use as parentHash for the next step.
func runPutStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string) (string, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content, err := merkle.PutNodeContent(cfg, step, *resourceType, resource.Source, step.Params, step.InputNames(), step.InputsAll())
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindPut, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "put", "resource", step.Put)

	fmt.Printf("put: %s\n", step.Put)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindPut, StepIndex: i, Resource: resource.Name, Content: content}

	result, err := executePut(ctx, cfg, step, bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
		recordStepFailure(ctx, st, node, jobName, wrapped)

		return "", wrapped
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", result, nil)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return hash, nil
}

// executePut materializes a put step's input view, runs its resource's out:
// command with retries and timeout, and returns the produced version — with
// no merkle/store recording. Shared by runPutStep (which records) and hook
// execution (which does not; a put hook's result version is discarded). A
// nonzero out: exit is marked as a task-level failure so hook dispatch
// classifies it as failed; a resource lookup or workspace error stays
// unmarked → errored.
func executePut(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (map[string]any, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}

	space, err := bw.PutSpace(ctx, step.Put, step.InputNames(), step.InputsAll())
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}
	defer workspace.CloseSpace(space, step.Put)

	return runPutWithRetry(ctx, cfg, step, *resourceType, resource.Source, space.Dir())
}

// runPutWithRetry executes a put step's out: command with retry/timeout support.
func runPutWithRetry(ctx context.Context, cfg *config.Config, step config.Step, resourceType config.ResourceType, source map[string]any, workspaceDir string) (map[string]any, error) {
	var result map[string]any

	retryErr := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		fmt.Printf("put: %s (attempt %d/%d)\n", step.Put, attempt, total)
		slog.Info("job.put.attempt", "put", step.Put, "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		runResult, runErr := rsrc.RunOut(attemptCtx, cfg, resourceType, source, step.Params, workspaceDir)
		if runErr != nil {
			// A canceled per-attempt context (timeout) or job-level abort must
			// propagate as-is rather than being marked a task-level Failure —
			// checked before IsExitError, since a killed process is also an
			// *exec.ExitError and would otherwise be misclassified as Failed
			// instead of Errored (see runTaskCommand's identical guard).
			cancelErr := shell.CanceledError(attemptCtx)
			if cancelErr != nil {
				return cancelErr //nolint:wrapcheck // wrapped with put context by the caller below
			}

			if shell.IsExitError(runErr) {
				runErr = outcome.Fail(runErr)
			}

			return runErr //nolint:wrapcheck // wrapped with put context by the caller below
		}

		result = runResult

		return nil
	})
	if retryErr != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, retryErr)
	}

	return result, nil
}

// runTriggeredBuild runs the build that a single resource version triggers:
// per Concourse's model, the version triggering a get is what starts a
// build, and every build gets its own isolated working directory. So this
// creates a fresh workspace for just this version, fetches the version
// into it, runs the remainder of the plan inside it, and tears the
// workspace down afterward — never sharing it with any other triggered
// build, including sibling versions fanned out by version:every.
func runTriggeredBuild(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, resource config.Resource,
	resourceType config.ResourceType, version map[string]any, remainder []config.Step, pinned map[string]string,
	provider workspace.Provider, st *store.Store, skippable map[string]bool, node merkle.Node,
	chainUnskippable bool, cache *rsrc.Cache,
) error {
	bw, err := provider.NewBuild(ctx, resource.Name)
	if err != nil {
		return fmt.Errorf("could not create workspace for %q: %w", resource.Name, err)
	}

	defer workspace.CloseBuild(bw, resource.Name)

	recordExecution(ctx, resource.Name)

	err = fetchGetStepWithStep(ctx, cfg, step, step.Get, resource, resourceType, version, bw)

	// Get-step hooks fire once per triggered build, in that build's own
	// workspace, observing the fetch outcome. A fetch failure (or a hook that
	// fails an otherwise-green fetch) fails this build.
	if !step.Hooks.Empty() {
		scope := hookScope{cfg: cfg, jobName: jobName, label: stepLabel(i, step), bw: bw}
		err = runHooks(ctx, scope, step.Hooks, err)
	}

	if err != nil {
		recordStepFailure(ctx, st, node, jobName, err)

		return err
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", nil, nil)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return runSteps(ctx, cfg, jobName, remainder, pinned, provider, bw, st, skippable, node.Hash, chainUnskippable, cache, false)
}

// fetchGetStep places one version of a resource into bw's resource directory,
// named for the get step's artifact name (its get: value) rather than the
// resource — they differ when the get aliases the resource via resource:. The
// directory, and thus the artifact downstream steps name as an input, is
// always the artifact name; only the fetched content comes from resource.
// fetchGetStepWithStep wraps fetchGetStep with retry/timeout support from the step.
func fetchGetStepWithStep(ctx context.Context, cfg *config.Config, step config.Step, artifact string, resource config.Resource, resourceType config.ResourceType, version map[string]any, bw workspace.BuildWorkspace) error {
	err := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		fmt.Printf("get: %s (version: %v, attempt %d/%d)\n", artifact, version, attempt, total)
		slog.Info("job.get.in.attempt", "artifact", artifact, "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		return fetchGetStep(attemptCtx, cfg, artifact, resource, resourceType, version, bw)
	})
	if err != nil {
		return fmt.Errorf("get %q: %w", artifact, err)
	}

	return nil
}

// resourceDir materializes artifact's directory and populates it with the
// given version, either by running the resource type's in: or — when the
// pipeline enabled the cross-build resource cache and this exact version has
// been fetched before — by reusing what an earlier build fetched.
//
// The cache key deliberately is NOT the get node's hash (see
// merkle.ResourceCacheKey): a node hash carries the step's position in a plan,
// so keying on it would give every job its own copy of identical bytes. A
// build workspace that cannot cache (the default shared one, or an isolating
// one with the cache off) takes the plain path, which is what every pipeline
// did before the cache existed.
func resourceDir(
	ctx context.Context, cfg *config.Config, artifact string,
	resourceType config.ResourceType, source, version map[string]any,
	bw workspace.BuildWorkspace, fetch func(dir string) error,
) error {
	caching, ok := bw.(workspace.CachingBuild)
	if !ok {
		dir, err := bw.ResourceDir(ctx, artifact)
		if err != nil {
			return fmt.Errorf("could not create resource dir for %q: %w", artifact, err)
		}

		return fetch(dir)
	}

	// A key this package cannot compute is not a reason to fail the fetch —
	// an empty key simply means "do not cache this one".
	key, err := merkle.ResourceCacheKey(cfg, resourceType, source, version)
	if err != nil {
		slog.Debug("job.get.cache_key_failed", "artifact", artifact, "error", err)

		key = ""
	}

	// Not wrapped with the artifact name: this error is either the fetch's own
	// (which the caller classifies as a task failure via IsExitError, a
	// judgement an extra wrap would not change but the linter cannot see) or
	// the workspace's, which already names the directory it failed on.
	_, err = caching.FetchResource(ctx, artifact, key, fetch)

	return err //nolint:wrapcheck // see above: the error is the caller-classified fetch error, passed through deliberately
}

func fetchGetStep(ctx context.Context, cfg *config.Config, artifact string, resource config.Resource, resourceType config.ResourceType, version map[string]any, bw workspace.BuildWorkspace) error {
	fmt.Printf("get: %s (version: %v)\n", artifact, version)

	err := resourceDir(ctx, cfg, artifact, resourceType, resource.Source, version, bw, func(dir string) error {
		return rsrc.RunIn(ctx, cfg, resourceType, resource.Source, version, dir)
	})
	if err != nil {
		// A canceled ctx (per-attempt timeout, or a job-level abort) must
		// propagate as-is rather than being marked a task-level Failure — a
		// killed process is also an *exec.ExitError, so this is checked before
		// IsExitError (see runTaskCommand's identical guard).
		cancelErr := shell.CanceledError(ctx)
		if cancelErr != nil {
			return fmt.Errorf("could not fetch resource %q: %w", resource.Name, cancelErr)
		}

		if shell.IsExitError(err) {
			err = outcome.Fail(err)
		}

		return fmt.Errorf("could not fetch resource %q: %w", resource.Name, err)
	}

	return nil
}

// jobBudgetTokens is a job's cumulative agent-token ceiling, or 0 for none.
// stepBudgetTokens is an across: block's token ceiling, or 0 when it has none.
//
// Alongside jobBudgetTokens rather than exported from config: the agent and job
// ceilings are read through private helpers there and a private one here, and
// exporting an accessor for this one alone both commits config's public API for
// a single entity and leaves two nil-checks to drift apart.
func stepBudgetTokens(step config.Step) int {
	if step.Budget == nil {
		return 0
	}

	return step.Budget.Tokens
}

func jobBudgetTokens(job *config.Job) int {
	if job.Budget == nil {
		return 0
	}

	return job.Budget.Tokens
}

// reportJobUsage prints what a job's agent steps cost, with the per-step
// breakdown.
//
// It runs whether or not a budget was set, and whether or not the job
// succeeded. Being able to see "341,204 tokens across 4 agent steps" is
// valuable on its own — it carries no correctness risk, and it is what tells
// an operator which ceilings are even sensible to set. A job with no agent
// steps prints nothing.
func reportJobUsage(jobName string, usage *agent.RunUsage) {
	steps := usage.Steps()
	if len(steps) == 0 {
		return
	}

	total := usage.Total()

	fmt.Printf("usage: %s tokens across %d agent step(s)\n", humanCount(total), len(steps))

	for _, step := range steps {
		fmt.Printf("  %-16s %s\n", step.Step, humanCount(step.Total))
	}

	fields := []any{"job", jobName, "total_tokens", total, "agent_steps", len(steps)}
	if budget := usage.Budget(); budget > 0 {
		fields = append(fields, "budget_tokens", budget)
	}

	slog.Info("job.usage", fields...)
}

// humanCount renders a token count with thousands separators, since the
// numbers involved are large enough that 341204 and 3412040 read alike.
func humanCount(n int) string {
	digits := strconv.Itoa(n)
	if len(digits) <= 3 {
		return digits
	}

	var out strings.Builder

	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}

		out.WriteRune(r)
	}

	return out.String()
}

// preflightKey types the context value that switches preflight off.
type preflightKey struct{}

// WithoutPreflight disables the pre-run health check for this invocation,
// backing the --no-preflight flag. On the context rather than a RunJob
// parameter because every caller in the chain would otherwise have to thread a
// flag it has no opinion about.
func WithoutPreflight(ctx context.Context) context.Context {
	return context.WithValue(ctx, preflightKey{}, true)
}

func preflightDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(preflightKey{}).(bool)

	return disabled
}

// Preflight probes every model and MCP server the job's plan reaches and
// reports the ones that are not working. It runs nothing and changes nothing.
//
// Exported so `steps preflight` can ask the question without committing to a
// run. The CLI layer reaches internal/agent through here rather than directly,
// keeping the dependency direction the depguard rules describe.
func Preflight(ctx context.Context, cfg *config.Config, job *config.Job) []config.Problem {
	names := job.AgentNames()
	if len(names) == 0 {
		return nil
	}

	settings := preflightSettings(cfg)
	if !settings.Enabled() {
		return nil
	}

	return agent.Preflight(ctx, cfg, names, settings)
}

// preflight proves the models and MCP servers this job's plan needs are
// actually working, before a single step runs.
//
// The failure it exists for is a plan like plan -> code -> check -> review
// -> publish discovering, half an hour and real money in, that a model was
// never going to answer. Under `steps watch` it is worse: nobody is watching,
// and a job re-triggers against a dead model indefinitely.
//
// A job with no agent steps checks nothing and costs nothing.
func preflight(ctx context.Context, cfg *config.Config, job *config.Job) error {
	if preflightDisabled(ctx) {
		return nil
	}

	problems := Preflight(ctx, cfg, job)
	if len(problems) == 0 {
		return nil
	}

	// Explicitly "no steps were run": the whole value of failing here rather
	// than mid-plan is that nothing was spent, and the message has to say so
	// or a reader cannot tell this from an ordinary step failure.
	var out strings.Builder

	fmt.Fprintf(&out, "job %q: preflight failed, no steps were run:", job.Name)

	for _, problem := range problems {
		fmt.Fprintf(&out, "\n  %s: %s", problem.Target, problem.Detail)
	}

	return errors.New(out.String())
}

func preflightSettings(cfg *config.Config) *config.Preflight {
	if cfg.Defaults == nil {
		return nil
	}

	return cfg.Defaults.Preflight
}

// ResetPreflightCache forgets everything preflight has verified in this
// process. Tests use it to stay independent of each other; nothing in a real
// run needs it, since the cache is bounded by its own TTL.
func ResetPreflightCache() { agent.ResetProbeCache() }
