package pipeline

// The in_parallel: branch runner.
//
// A plan is otherwise strictly sequential, so independent work waits on itself:
// three downloads run one at a time, and one slow resource check stalls
// everything behind it. This is also the keystone the aggregation policies
// build on — across:, ensemble: and race: are all "run these branches, decide
// what their outcomes mean".

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// branchResult is one branch's outcome, kept with its index so the report
// reads in declaration order however the branches actually finished.
type branchResult struct {
	index int
	name  string
	err   error
}

// runParallelStep runs an in_parallel: block's branches concurrently and
// reports the block's outcome.
//
// The block FAILS when any branch fails. fail_fast decides only whether the
// siblings are cancelled or allowed to finish — never whether the failure
// counts. That distinction is the defect this step shipped with the first time
// it was written: with fail_fast: false a child failure was swallowed and a job
// containing a failing parallel step reported PASS.
func runParallelStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	content, err := merkle.ParallelNodeContent(cfg, step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindParallel, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	branches := step.InParallel.Steps

	fmt.Printf("in_parallel: %d branches%s\n", len(branches), limitSuffix(step.InParallel.Limit))
	slog.Debug("job.step", "job", jobName, "index", i, "kind", "in_parallel", "branches", len(branches))

	results := runBranches(ctx, cfg, jobName, i, step, bw, st, hash, handoff)
	blockErr := combineBranchErrors(ctx, results)

	status := "succeeded"
	if blockErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindParallel,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, status, nil, blockErr)

	return hash, stepRan, nonGetOutcome{}, blockErr
}

// runBranches executes every branch, bounded by limit, and collects one result
// per branch.
//
// Each branch chains under the BLOCK's hash rather than under its siblings:
// concurrent branches have no order between them, so there is no sequence for
// one to be the parent of another. Caching is off inside the block (nil
// skippable) — a branch's own steps still record their nodes, but the block
// decides nothing about skipping.
func runBranches(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, blockHash string, handoff *agent.Handoff,
) []branchResult {
	branches := step.InParallel.Steps
	results := make([]branchResult, len(branches))

	// fail_fast cancels the siblings of a failing branch. Derived from ctx so
	// an outer cancellation (a job abort) still reaches every branch.
	branchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		slot = newLimiter(step.InParallel.Limit, len(branches))
		logs = make([]*execLog, len(branches))
	)

	// Slots are acquired HERE, in the parent, rather than inside each
	// goroutine. That makes branches start in declaration order — under
	// `limit:` especially, "which two go first" is otherwise whichever
	// goroutines the scheduler happened to run, which is nothing a pipeline
	// author can reason about.
	// Identity is stamped here, in the parent, rather than inside the
	// goroutine: under fail_fast a branch may never start, and a zero-value
	// result would then claim index 0 — which the context merge below reads
	// as branch 0's scope. Every branch now carries its own index whether or
	// not it ran; one that never ran simply has no rows to merge.
	for index := range branches {
		results[index] = branchResult{index: index, name: executedStepName(branches[index])}
	}

	for index := range branches {
		slot.acquire()

		// Under fail_fast a branch that has not started yet never should.
		if branchCtx.Err() != nil {
			slot.release()

			break
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer slot.release()

			branch := branches[index]

			// Each branch records into its own log, merged back in
			// declaration order below — branches finish in whatever order
			// they finish, and recording as they go would make
			// assert.execution nondeterministic.
			runCtx, branchLog := forkExecLog(branchCtx)
			logs[index] = branchLog

			// Context writes go to a scope only this branch touches; they are
			// merged back at the join below, in declaration order. Writing
			// straight into the run would make two branches recording one key
			// resolve to whichever finished last — the hazard
			// validateParallelOutputs already refuses for artifact names.
			//
			// Derived from the ENCLOSING scope, not from the run: a nested
			// block inside two different branches would otherwise compute the
			// same scope for its own branch 0 and the two would overwrite each
			// other's rows before either join saw them.
			runCtx = agent.WithContextScope(runCtx,
				branchContextScope(agent.ContextWriteScope(ctx), index, results[index].name))

			_, _, _, err := runNonGetStep(runCtx, cfg, jobName, i, branch, bw, st, nil, blockHash, handoff)

			// A try: branch tolerates its own failure HERE, because the plan
			// walk never sees a branch — executeNonGetStep is where every
			// other try: is tolerated, and runNonGetStep is as far up as a
			// branch goes. Without this the identical wrapper was tolerated
			// in a serial plan and propagated inside a block, which is the
			// case try: is most often reached for: a best-effort notification
			// running alongside the work it reports on.
			//
			// Before the fail_fast check, so a tolerated failure does not
			// cancel its siblings either. It is not a failure any more.
			err = tolerateTryFailure(runCtx, jobName, branch, err)

			results[index].err = err

			if err != nil && step.InParallel.FailsFast() {
				cancel()
			}
		}()
	}

	wg.Wait()

	for _, branchLog := range logs {
		if branchLog != nil {
			mergeExecLog(ctx, branchLog)
		}
	}

	mergeBranchesContext(ctx, st, results)

	return results
}

// mergeBranchesContext folds every branch's recorded facts back into the scope
// the block itself writes to, in declaration order, once they have all
// finished.
//
// Into the ENCLOSING scope rather than into the run: a nested block runs
// inside its own branch's goroutine, so merging straight into the run would
// put two concurrent writers on the run's rows — the very race the branch
// scopes exist to remove. Merging one level at a time means the outer join
// carries the inner facts the rest of the way up, still single-threaded.
//
// Order and single-threadedness are the whole point: this is what turns
// concurrent writes into a deterministic result. A failure to merge is logged
// rather than raised — the branches' own outcomes are the block's outcome, and
// losing a recorded fact must not turn a green block red.
func mergeBranchesContext(ctx context.Context, st *store.Store, results []branchResult) {
	enclosing := agent.ContextWriteScope(ctx)

	// Resolved over the whole set before any of it is written: two branch names
	// can sanitize to one prefix, and only something holding every sibling can
	// tell.
	prefixes := branchPrefixes(results)

	for i, result := range results {
		// An unnamed branch is one that IS a block: executedStepName has no
		// name for a container. It still has to be merged, or the facts its
		// own join lifted into it stay one level below the run and no later
		// step ever sees them — and it still needs an identity, or two such
		// branches collapse onto one key and the second silently wins.
		err := mergeBranchContext(
			context.WithoutCancel(ctx), st, enclosing,
			branchContextScope(enclosing, result.index, result.name),
			branchPrefixName(result), prefixes[i],
		)
		if err != nil {
			slog.Warn("branch.context.merge_failed", "branch", result.name, "error", err)
		}
	}
}

// branchPrefixName is the name a branch's recorded facts are qualified by: its
// step name, or its position when it has none (a nested block).
func branchPrefixName(result branchResult) string {
	if result.name != "" {
		return result.name
	}

	return fmt.Sprintf("branch%d", result.index)
}

// combineBranchErrors turns the branches' outcomes into the block's own.
//
// Every failure is reported, not just the first: a reader debugging a parallel
// block wants to know whether one branch broke or all of them did, and a
// truncated report hides exactly that. The block's classification follows the
// worst branch — an errored branch (infrastructure) outranks a failed one (the
// step said no), so on_error fires rather than on_failure.
func combineBranchErrors(ctx context.Context, results []branchResult) error {
	var (
		failures []error
		errored  bool
	)

	for _, result := range results {
		if result.err == nil {
			continue
		}

		failures = append(failures, fmt.Errorf("branch %d (%s): %w", result.index, result.name, result.err))

		if outcome.Classify(ctx, result.err) != outcome.Failed {
			errored = true
		}
	}

	if len(failures) == 0 {
		return nil
	}

	joined := errors.Join(failures...)
	if errored {
		return joined
	}

	// Every failure was a step-level one, so the block is a step-level failure
	// too and on_failure is the right hook.
	return outcome.Fail(joined) //nolint:wrapcheck // Fail only marks the classification; the joined error is already this package's
}

// limiter bounds how many branches are in flight. A zero or oversized limit
// means unbounded — the plan author asked for parallelism, so making them also
// choose a width would be a second decision for the common case.
type limiter struct{ slots chan struct{} }

func newLimiter(limit, branches int) *limiter {
	if limit <= 0 || limit >= branches {
		return &limiter{}
	}

	return &limiter{slots: make(chan struct{}, limit)}
}

func (l *limiter) acquire() {
	if l.slots != nil {
		l.slots <- struct{}{}
	}
}

func (l *limiter) release() {
	if l.slots != nil {
		<-l.slots
	}
}

func limitSuffix(limit int) string {
	if limit <= 0 {
		return ""
	}

	return fmt.Sprintf(" (limit %d)", limit)
}

// runRaceStep runs a race: block's branches concurrently and keeps whichever
// completes successfully first, cancelling the rest.
//
// "Successfully" means completed without error — NOT a judgment about output
// quality. A fast but mediocre answer still wins; gating on quality is a
// downstream assert:/verdicts: concern, and folding it in here would make the
// race's outcome depend on something no branch can observe about itself.
//
// Losing branches are cancelled, which stops only FUTURE work: a loser that
// already had a real-world side effect keeps it. That is why race: requires
// workspace isolation and is documented as safe for read/generate-only agents.
func runRaceStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	content, err := merkle.RaceNodeContent(cfg, step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (race): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindRace, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (race): %w", i, err)
	}

	branches := step.Race.Steps

	fmt.Printf("race: %d branches\n", len(branches))
	slog.Debug("job.step", "job", jobName, "index", i, "kind", "race", "branches", len(branches))

	winner, results := raceBranches(ctx, cfg, jobName, i, branches, bw, st, hash, handoff)
	raceErr := raceOutcome(ctx, winner, results)

	status := "succeeded"
	if raceErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindRace,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, status, nil, raceErr)

	return hash, stepRan, nonGetOutcome{}, raceErr
}

// raceBranches starts every branch and returns the index of the first to
// succeed (-1 if none did) along with every branch's result.
func raceBranches(
	ctx context.Context, cfg *config.Config, jobName string, i int, branches []config.Step,
	bw workspace.BuildWorkspace, st *store.Store, blockHash string, handoff *agent.Handoff,
) (int, []branchResult) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winner  = -1
		results = make([]branchResult, len(branches))
		logs    = make([]*execLog, len(branches))
	)

	for index := range branches {
		wg.Add(1)

		go func() {
			defer wg.Done()

			branch := branches[index]
			results[index] = branchResult{index: index, name: executedStepName(branch)}

			runCtx, branchLog := forkExecLog(raceCtx)
			logs[index] = branchLog

			// Scoped like an in_parallel: branch, but only the winner's scope
			// is merged below — a cancelled loser's partial facts are discarded
			// with its workspace, the same treatment its exec log gets.
			runCtx = agent.WithContextScope(runCtx,
				branchContextScope(agent.ContextWriteScope(ctx), index, results[index].name))

			_, _, _, err := runNonGetStep(runCtx, cfg, jobName, i, branch, bw, st, nil, blockHash, handoff)
			results[index].err = err

			if err != nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// First success wins; a later one changes nothing, and cancelling
			// again is harmless.
			if winner < 0 {
				winner = index
			}

			cancel()
		}()
	}

	wg.Wait()

	// Only the winner's execution is recorded. A cancelled loser's partial
	// work is not something a fixture should have to predict, and its
	// artifacts are discarded.
	if winner >= 0 && logs[winner] != nil {
		mergeExecLog(ctx, logs[winner])
	}

	if winner >= 0 {
		mergeBranchesContext(ctx, st, results[winner:winner+1])
	}

	return winner, results
}

// raceOutcome reports the block's error: none when some branch won, and every
// branch's failure when none did.
func raceOutcome(ctx context.Context, winner int, results []branchResult) error {
	if winner >= 0 {
		return nil
	}

	return combineBranchErrors(ctx, results)
}
