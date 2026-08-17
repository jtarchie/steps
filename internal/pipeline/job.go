package pipeline

// A job run's lifecycle: validate, preflight, walk the plan, dispatch job
// hooks, and report what it spent.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

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
	// A run identifies itself so a failure can be continued rather than
	// restarted, and so every log line below — including this one — can be
	// correlated to one invocation. Minted first, before validation or
	// preflight even run: a run that fails before its first step is still a
	// run somebody may want to trace.
	resume := resumeFrom(ctx)
	if resume == nil {
		resume = &resumeState{id: NewRunID(), done: map[int]string{}}
		ctx = withResume(ctx, resume)
	}

	// Publish the run's identity where packages that cannot import this one
	// can still stamp it on their events — internal/agent, tagging every
	// conversation turn with the run it belongs to.
	ctx = events.WithRunID(ctx, resume.id)

	ctx = withRunLogger(ctx, resume.id, job.Name)

	logFrom(ctx).Info("job.run", "steps", len(job.Plan))

	err := workspace.ValidateArtifactFlow(cfg, job)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	// One register of step decisions per job run, for context: from: readers.
	// Installed here rather than in runSteps because a get: version: every
	// fan-out re-enters runSteps per version, and a decision made before the
	// get is still this run's.
	ctx = agent.WithOutcomes(ctx)

	err = prepareImages(ctx, cfg, job.Name)
	if err != nil {
		return err
	}

	err = preflight(ctx, cfg, job)
	if err != nil {
		return err
	}

	bw, err := provider.NewBuild(ctx, job.Name)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	// Retention, registered BEFORE the event bus so it runs AFTER closeBus has
	// flushed: what links a node to a run is an event naming its hash, and
	// pruning while events were still queued would let a resumed run's older
	// nodes look unreferenced.
	//
	// At the end of every build rather than on a timer, because a watch process
	// may never idle, and the moment one run finishes is the moment its
	// predecessors became prunable.
	defer pruneHistory(ctx, st, cfg, job.Name, resume.id)

	// Every run records its own story, whether or not anything is watching.
	ctx, closeBus := attachEventBus(ctx, st)
	defer closeBus()

	// Recorded for every build, not only a rooted one: the row is this run's
	// identity — what its events, its history entry, and its resume all key
	// on. A backend that exposes no root directory records an empty workspace.
	workspaceRoot := ""
	if rooted, ok := bw.(workspace.RootedBuild); ok {
		workspaceRoot = rooted.Root()
	}

	_ = st.StartRun(ctx, resume.id, job.Name, workspaceRoot)

	jobStarted := time.Now()

	publishJobStarted(ctx, job.Name)

	// Carry an execution log through this invocation so a job's assert.execution
	// can self-verify what ran (plan steps and hooks). The dispatch points and
	// runHookStep append to it; nothing outside pipeline touches it.
	log := &execLog{}
	ctx = withExecLog(ctx, log)

	ctx = withRunContext(ctx, job, skipCache)

	// Remember which versions this run fetched, so a successful job can mark
	// them green for any downstream job's passed: constraint. Every fetch
	// happens inside a triggered build, which installs its own record over
	// this one (runTriggeredBuild) — this is the outer fallback, and normally
	// stays empty.
	ctx, fetched := withFetchedVersions(ctx)

	// Account for what this job's agent steps spend, and enforce the job's
	// cumulative ceiling if it set one. Installed here, not per step, because
	// a job budget is by definition the sum across steps.
	//
	// A RESUMED run continues from what its earlier attempts already spent
	// (agent_usage, keyed by the run id this invocation reuses). Starting at
	// zero would make budget: a per-attempt ceiling wearing the name of a
	// per-run one, and buy another full allowance on every resume.
	usage := agent.NewResumedRunUsage(jobBudgetTokens(job), priorSpend(ctx, st, resume))
	ctx = agent.WithRunUsage(ctx, usage)

	defer reportJobUsage(ctx, usage)

	runner := stepRunner{cfg: cfg, jobName: job.Name, bw: bw, st: st}

	// Everything from here on has a workspace to run job-level hooks in, so
	// funnel planning and execution into one outcome and dispatch the job's
	// hooks around it. Pre-workspace failures above fire no hooks — there's
	// nowhere for a job-level hook to run yet. Job hooks fire on every
	// invocation past this point, cached or not; they are never hashed or
	// skipped. Concourse has no job-level hook construct to compare against
	// (its hooks are step modifiers) — see docs/conformance.md.
	runErr := runJobPlan(ctx, runner, job, pinned, provider, skipCache)

	finalErr := runHooks(ctx, runner.scope(fmt.Sprintf("job %q", job.Name)), job.Hooks, runErr)

	// A job assert is the final word: it runs after hooks so the log includes
	// them. A mismatch fails the job regardless, and is never itself cleared.
	finalErr = checkJobAssert(job, log, finalErr)
	if finalErr != nil {
		// Keep the workspace on failure rather than destroying it: the files a
		// step had just written when it failed are the most useful thing to
		// look at, and they are what a resume continues from.
		_ = st.FinishRun(context.WithoutCancel(ctx), resume.id, "failed")

		publishJobFinished(ctx, job.Name, jobStarted, finalErr)
		reportResumable(resume.id, bw)

		return finalErr
	}

	workspace.CloseBuild(bw, job.Name)

	_ = st.FinishRun(context.WithoutCancel(ctx), resume.id, "succeeded")

	publishJobFinished(ctx, job.Name, jobStarted, nil)

	recordPassedVersions(ctx, st, job.Name, resume.id, fetched)

	logFrom(ctx).Info("job.done")

	return nil
}

// withRunContext installs the per-invocation switches the runners several
// frames down read off the context: the OpenRouter session that keeps an
// agent's prompt cache warm, --force, load_var: scoping, and the job's
// wall-clock ceiling.
//
// The session is scoped to this invocation so concurrent jobs under
// `steps watch --max-concurrent` never share a provider pin; non-OpenRouter
// providers ignore it outright. --force has to reach the runners that keep
// their own cache rather than consulting the chain index (across: cells).
// A load_var: value says nothing about the next run, so it is scoped here too.
func withRunContext(ctx context.Context, job *config.Job, skipCache bool) context.Context {
	ctx = agent.WithNewRun(ctx, job.Name)
	ctx = withForce(ctx, skipCache)
	ctx = withRunVars(ctx)

	return withJobDeadline(ctx, job)
}

// prepareImages validates the Docker daemon and warms every image the
// pipeline names, BEFORE preflight — which asks docker questions of its own
// (a containerized CLI agent is probed by running its image). Probing first
// meant that probe hit an unvalidated daemon and an unpulled image, so a
// missing daemon was reported as "this image cannot run the cli" and a cold
// image was pulled inside the 30s probe timeout instead of here.
//
// Present images are a local inspect, so a warm run — including every
// subsequent job under `steps watch` — costs nothing.
func prepareImages(ctx context.Context, cfg *config.Config, jobName string) error {
	if !cfg.UsesImages() {
		return nil
	}

	err := shell.ValidateDocker(ctx)
	if err != nil {
		return fmt.Errorf("job %q: image: configured but docker is unavailable: %w", jobName, err)
	}

	// Reclaim containers a previous run was SIGKILLed before it could remove.
	// Best-effort and silent when there is nothing to do.
	shell.SweepOrphanedContainers(ctx)

	err = shell.PrepareImages(ctx, cfg.Images())
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	return nil
}

// runJobPlan plans (unless skipCache) and runs a job's steps, returning the
// aggregate outcome that job-level hooks dispatch on. A planning failure
// classifies as errored (job on_error); a step failure carries whatever
// classification its producing site marked it with.
func runJobPlan(
	ctx context.Context, r stepRunner, job *config.Job, pinned map[string]string,
	provider workspace.Provider, skipCache bool,
) error {
	// Which versions this job has already fanned out over, read ONCE before
	// planning so the planner and the executor judge the same set.
	//
	// --force (skipCache) stops the cursor SUPPRESSING versions — "re-run
	// every step" has to include the ones a previous run took, or the flag
	// cannot recover from a bad build — but the cursor is still built and
	// still records. Skipping the recording too would mean a forced run
	// performed every effect and remembered none of them.
	// Whatever triggered this run, its resources are re-checked first — see
	// refreshResourceHistory. Best-effort by design: the resolution below
	// builds from recorded history either way.
	refreshResourceHistory(ctx, r.cfg, r.st, job)

	cursor, err := loadVersionCursor(ctx, r.st, job, !skipCache)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	// cache is scoped to this one RunJob invocation (never shared across
	// concurrent invocations — see resource.NewCache) and threaded into both
	// the plan-time and run-time get-step resolution below, so a get step's
	// check command runs at most once per job run instead of once during
	// planning and again during execution.
	// Which versions each of this job's resources has, read once before
	// planning for the same reason the consumed set is.
	history, err := loadResourceHistory(ctx, r.st, job)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	cache := rsrc.NewCache(rsrc.WithConsumed(cursor.has), rsrc.WithResolvedVersions(history.get))

	// The input sets this run will build — one per unconsumed step of the
	// widest every-get, each binding EVERY get's version. Computed once,
	// before planning and unconditionally (--force skips planning, but the
	// walk still fans out over these), then walked identically by the planner
	// and the executor, which is what keeps the two describing one shape.
	resolution, err := resolveInputSets(ctx, r.cfg, job.Plan, pinned, cache, cursor, history)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	skippable := map[string]bool{}

	if !skipCache {
		chains, planErr := merkle.PlanChains(ctx, r.cfg, job.Name, job.Plan, pinned, cache, resolution.sets)
		if planErr != nil {
			return fmt.Errorf("job %q: planning: %w", job.Name, planErr)
		}

		skippable, err = buildSkippableIndex(ctx, r.st, job.Name, chains)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}
	}

	return runSteps(ctx, planWalk{
		stepRunner:      r,
		pinned:          pinned,
		provider:        provider,
		skippable:       skippable,
		cache:           cache,
		cursor:          cursor,
		resolution:      resolution,
		allowGetTrigger: true,
	}, job.Plan)
}

// pruneHistory trims what this job has accumulated: runs past the cap, with
// their events, steps, usage and transcripts, and the trigger-queue rows for
// work already finished.
//
// Best-effort and deliberately so. A database that could not be trimmed is not
// a reason to fail a build that already succeeded — the failure is logged, the
// next build tries again, and the only cost of a missed pass is the bytes it
// would have freed.
//
// Runs on a detached context: a canceled or timed-out job is exactly the one
// whose history is worth bounding, and the ambient context is already done by
// the time this fires.
// keepRunID is this build's own run, which retention must never delete — see
// store.PruneRuns, where a resumed run reaped itself.
func pruneHistory(ctx context.Context, st *store.Store, cfg *config.Config, jobName, keepRunID string) {
	pruneCtx := context.WithoutCancel(ctx)

	err := st.PruneRuns(pruneCtx, jobName, cfg.RunHistoryLimit(), keepRunID)
	if err != nil {
		logFrom(ctx).Warn("store.prune_runs", "job", jobName, "error", err)
	}

	err = st.PruneTriggerQueue(pruneCtx, jobName, store.DefaultTriggerQueueHistory)
	if err != nil {
		logFrom(ctx).Warn("store.prune_trigger_queue", "job", jobName, "error", err)
	}
}

// recordChainSucceeded records the leaf of a fully-executed chain as
// succeeded, unless it contains a put or agent step (those chains are
// never skippable, so recording job_runs for them would be unused).
func recordChainSucceeded(ctx context.Context, r stepRunner, rootHash string, chainUnskippable bool) error {
	if chainUnskippable {
		return nil
	}

	err := r.st.RecordJobRun(ctx, r.jobName, rootHash, "succeeded", nil)
	if err != nil {
		return fmt.Errorf("job %q: %w", r.jobName, err)
	}

	return nil
}

// stepBudgetTokens is an across: block's token ceiling, and jobBudgetTokens a
// job's cumulative one; 0 means none.
//
// Private here rather than exported from config: the agent and job ceilings
// are read through private helpers there and a private one here, and
// exporting an accessor for this one alone both commits config's public API
// for a single entity and leaves two nil-checks to drift apart.
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

// priorSpend is what earlier attempts of this run already spent, for a resumed
// run; 0 for a fresh one.
//
// Best-effort by design: a store that cannot answer must not stop the run.
// Failing open costs at most one budget's overshoot on a resume, where failing
// closed would refuse to continue work that has already been paid for.
func priorSpend(ctx context.Context, st *store.Store, resume *resumeState) int {
	if st == nil || resume == nil || !resume.resuming {
		return 0
	}

	spent, err := st.RunTokensSpent(ctx, resume.id)
	if err != nil {
		slog.Warn("run.resume.prior_spend_unavailable", "run", resume.id, "error", err,
			"detail", "the job budget will start from zero for this attempt")

		return 0
	}

	if spent > 0 {
		slog.Info("run.resume.prior_spend", "run", resume.id, "tokens", spent)
	}

	return spent
}

// reportJobUsage prints what a job's agent steps cost, with the per-step
// breakdown. It runs whether or not a budget was set and whether or not the
// job succeeded: seeing "341,204 tokens across 4 agent steps" is what tells an
// operator which ceilings are even sensible to set. A job with no agent steps
// prints nothing.
func reportJobUsage(ctx context.Context, usage *agent.RunUsage) {
	steps := usage.Steps()
	prior := usage.Prior()

	if len(steps) == 0 && prior == 0 {
		return
	}

	total := usage.Total()

	// The per-step lines below are THIS attempt's, so a resumed run says what
	// the earlier attempts contributed rather than printing a total the
	// listed steps do not add up to.
	if prior > 0 {
		fmt.Printf("usage: %s tokens across %d agent step(s) this attempt, %s total for the run (%s from earlier attempts)\n",
			humanCount(total-prior), len(steps), humanCount(total), humanCount(prior))
	} else {
		fmt.Printf("usage: %s tokens across %d agent step(s)\n", humanCount(total), len(steps))
	}

	for _, step := range steps {
		fmt.Printf("  %-16s %s\n", step.Step, humanCount(step.Total))
	}

	fields := []any{"total_tokens", total, "agent_steps", len(steps)}
	if prior > 0 {
		fields = append(fields, "prior_tokens", prior, "attempt_tokens", total-prior)
	}

	if budget := usage.Budget(); budget > 0 {
		fields = append(fields, "budget_tokens", budget)
	}

	logFrom(ctx).Info("job.usage", fields...)
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
