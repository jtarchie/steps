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
	"slices"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
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
func runParallelStep(ctx context.Context, r stepRunner, i int, step config.Step, node merkle.Node) (stepResult, error) {
	hash := node.Hash
	branches := step.InParallel.Steps

	slog.DebugContext(ctx, "job.step", "job", r.jobName, "index", i, "kind", "in_parallel", "branches", len(branches))

	results := runBranches(ctx, r, i, step, hash)
	blockErr := combineBranchErrors(ctx, results)

	recordBlock(ctx, r, node, nil, blockErr)

	return settled(hash, blockErr), blockErr
}

// runBranches executes every branch, bounded by limit, and collects one result
// per branch.
//
// Each branch chains under the BLOCK's hash rather than under its siblings:
// concurrent branches have no order between them, so there is no sequence for
// one to be the parent of another. Caching is off inside the block (nil
// skippable) — a branch's own steps still record their nodes, but the block
// decides nothing about skipping.
func runBranches(ctx context.Context, r stepRunner, i int, step config.Step, blockHash string) []branchResult {
	branches := step.InParallel.Steps
	results := make([]branchResult, len(branches))

	// fail_fast cancels the siblings of a failing branch. Derived from ctx so
	// an outer cancellation (a job abort) still reaches every branch.
	branchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Identity is stamped here, in the parent, rather than inside the
	// goroutine: under fail_fast a branch may never start, and a zero-value
	// result would then wrongly claim index 0. Every branch now carries its
	// own index whether or not it ran.
	for index := range branches {
		results[index] = branchResult{index: index, name: executedStepName(branches[index])}
	}

	fanOut(branchCtx, len(branches), step.InParallel.Limit,
		func(index int) bool {
			// Under fail_fast a branch that has not started yet never should.
			//
			// Nor should one the job no longer has time for: the plan walk checks the deadline between STEPS, and this whole block is one of them, so without asking here a job timeout: cannot bound a parallel fan-out at all — the same hole across: had, closed the same way and with the same helper.
			return branchCtx.Err() == nil && !deadlineStopsFanOut(ctx, r.jobName, "in_parallel", "branches", index, len(branches))
		},
		func(runCtx context.Context, index int) {
			branch := branches[index]

			branchRes, err := runNonGetStep(runCtx, r, i, branch, nil, blockHash)

			// A try: branch tolerates its own failure HERE, because the plan
			// walk never sees a branch — planWalk.runStep is where every
			// other try: is tolerated, and runNonGetStep is as far up as a
			// branch goes. Without this the identical wrapper was tolerated
			// in a serial plan and propagated inside a block, which is the
			// case try: is most often reached for: a best-effort notification
			// running alongside the work it reports on.
			//
			// Before the fail_fast check, so a tolerated failure does not
			// cancel its siblings either. It is not a failure any more.
			err = tolerateTryFailure(runCtx, r.jobName, branch, branchRes.stepID, err)

			results[index].err = err

			if err != nil && step.InParallel.FailsFast() {
				cancel()
			}
		},
		func(index int, err error) {
			results[index].err = err

			if step.InParallel.FailsFast() {
				cancel()
			}
		},
	)

	return results
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

	return joinWorst(failures, errored)
}

// joinWorst is a block's error from its children's: errored when any child errored, which outranks the ones that only said no, and failed otherwise, so on_failure is the right hook.
func joinWorst(failures []error, errored bool) error {
	if len(failures) == 0 {
		return nil
	}

	joined := errors.Join(failures...)
	if errored {
		// Escalated, not bare: the join holds the failed children's markers too, and the first one errors.As met decided the block was failed.
		return outcome.Escalate(joined) //nolint:wrapcheck // Escalate only marks the classification
	}

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
func runRaceStep(ctx context.Context, r stepRunner, i int, step config.Step, node merkle.Node) (stepResult, error) {
	hash := node.Hash
	branches := step.Race.Steps

	slog.DebugContext(ctx, "job.step", "job", r.jobName, "index", i, "kind", "race", "branches", len(branches))

	winner, results := raceBranches(ctx, r, i, branches, hash)
	raceErr := raceOutcome(ctx, winner, results)

	recordBlock(ctx, r, node, nil, raceErr)

	return settled(hash, raceErr), raceErr
}

// raceBranches starts every branch and returns the index of the first to
// succeed (-1 if none did) along with every branch's result. Which one that is
// is the gate's call, since the first to write the race's outputs holds them
// until it finishes (see raceGate).
func raceBranches(ctx context.Context, r stepRunner, i int, branches []config.Step, blockHash string) (int, []branchResult) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Stamped in the parent, so a branch that panics before it gets going still reports under its own name.
	results := make([]branchResult, len(branches))
	for index := range branches {
		results[index] = branchResult{index: index, name: executedStepName(branches[index])}
	}

	gate := newRaceGate(results, cancel)

	logs := fanOutLogged(raceCtx, len(branches), 0,
		func(int) bool { return true },
		func(runCtx context.Context, index int) {
			_, err := runNonGetStep(withRaceSeat(runCtx, gate, index), r, i, branches[index], nil, blockHash)
			results[index].err = err

			gate.finish(runCtx, index, err == nil)
		},
		func(index int, err error) {
			results[index].err = err

			gate.finish(raceCtx, index, false)
		},
	)

	// Only the winner's execution is recorded. A cancelled loser's partial
	// work is not something a fixture should have to predict, and its
	// artifacts are discarded.
	winner := gate.won()
	if winner >= 0 && logs[winner] != nil {
		mergeExecLog(ctx, logs[winner])
	}

	return winner, results
}

// errRaceLost is a branch refused the race's outputs because another branch won.
var errRaceLost = errors.New("another branch of the race won; its outputs are kept")

// errRaceHeld is a claim that would have had to wait for another branch.
var errRaceHeld = errors.New("another branch of the race holds its outputs")

// raceGate decides a race and serializes what its branches write to the build's artifacts behind that decision: a capture ignores cancellation, so a loser finishing after the winner wrote would replace the winner's outputs with its own.
type raceGate struct {
	mu     sync.Mutex
	turn   *sync.Cond
	names  []string
	cancel context.CancelFunc
	// owner is the branch holding the artifacts until it finishes, -1 for none; winner is the decided branch, -1 until then.
	owner, winner int
}

func newRaceGate(results []branchResult, cancel context.CancelFunc) *raceGate {
	names := make([]string, len(results))
	for index, result := range results {
		names[index] = result.name
	}

	gate := &raceGate{names: names, cancel: cancel, owner: -1, winner: -1}
	gate.turn = sync.NewCond(&gate.mu)

	return gate
}

// waits reports whether branch must let another settle first; called with mu held.
func (g *raceGate) waits(branch int) bool {
	return g.winner < 0 && g.owner >= 0 && g.owner != branch
}

// await blocks branch until no other branch holds the artifacts undecided, saying so, since a branch whose work is done would otherwise sit silent; called with mu held.
func (g *raceGate) await(ctx context.Context, branch int) {
	if g.waits(branch) {
		notef(ctx, "race: %s holds the race's outputs; %s waits for its outcome", g.names[g.owner], g.names[branch])
	}

	for g.waits(branch) {
		g.turn.Wait()
	}
}

// claim gives branch the artifacts, waiting while another branch holds them unless told not to (errRaceHeld); it reports whether this call took them, and errRaceLost once another branch has won.
func (g *raceGate) claim(ctx context.Context, branch int, wait bool) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !wait && g.waits(branch) {
		return false, errRaceHeld
	}

	g.await(ctx, branch)

	if g.winner >= 0 && g.winner != branch {
		return false, errRaceLost
	}

	took := g.owner != branch
	g.owner = branch

	return took, nil
}

// release gives back what claim took, for a cache lookup that restored nothing.
func (g *raceGate) release(branch int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.owner == branch && g.winner < 0 {
		g.owner = -1
		g.turn.Broadcast()
	}
}

// finish settles branch: a success wins unless another branch holds the artifacts, whose outcome is decided first; a failure gives them back. The losers are cancelled before anyone waiting is woken, so a refused branch classifies as a cancelled one does.
func (g *raceGate) finish(ctx context.Context, branch int, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	defer g.turn.Broadcast()

	if !ok {
		if g.owner == branch {
			g.owner = -1
		}

		return
	}

	g.await(ctx, branch)

	if g.winner < 0 {
		g.winner = branch
		g.cancel()
	}
}

func (g *raceGate) won() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.winner
}

// raceSeat is one race a step runs inside, and which of its branches.
type raceSeat struct {
	gate   *raceGate
	branch int
}

type raceSeatsKey struct{}

// withRaceSeat adds branch of gate to the races ctx runs inside, innermost last.
func withRaceSeat(ctx context.Context, gate *raceGate, branch int) context.Context {
	seats, _ := ctx.Value(raceSeatsKey{}).([]raceSeat)

	return context.WithValue(ctx, raceSeatsKey{}, append(slices.Clip(seats), raceSeat{gate: gate, branch: branch}))
}

// holdArtifacts waits until the step may write the build's artifacts — at once outside a race:, and never when !wait — and returns how to give back what it took, for a write that did not happen. Taken outermost first, so nested races wait in one order.
func holdArtifacts(ctx context.Context, wait bool) (func(), error) {
	seats, _ := ctx.Value(raceSeatsKey{}).([]raceSeat)
	taken := make([]raceSeat, 0, len(seats))

	release := func() {
		for _, seat := range slices.Backward(taken) {
			seat.gate.release(seat.branch)
		}
	}

	for _, seat := range seats {
		took, err := seat.gate.claim(ctx, seat.branch, wait)
		if err != nil {
			release()

			return func() {}, err
		}

		if took {
			taken = append(taken, seat)
		}
	}

	return release, nil
}

// captureHeld runs capture once the step may write the build's artifacts (holdArtifacts).
func captureHeld(ctx context.Context, capture func(context.Context) error) error {
	_, err := holdArtifacts(ctx, true)
	if err != nil {
		return err
	}

	return capture(ctx)
}

// raceOutcome reports the block's error: none when some branch won, and every
// branch's failure when none did.
func raceOutcome(ctx context.Context, winner int, results []branchResult) error {
	if winner >= 0 {
		return nil
	}

	return combineBranchErrors(ctx, results)
}
