package pipeline

// An across: block's token allowance: what its cells may spend together.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
)

// blockBudget is an across: block's token allowance: what its cells may spend
// TOGETHER, and how much of it is left.
//
// Measured as a delta against the job's own accumulator rather than a counter
// of its own — every agent step already rolls its provider-reported usage up
// into RunUsage, so the matrix's spend is simply what the job's total moved by
// while the block ran. No new plumbing, and it counts a cell's retries and
// sub-agents for free, since those roll up the same way.
//
// A zero budget (the common case) is a nil ceiling that never trips.
//
// Admission RESERVES against the ceiling rather than only reading what
// finished cells reported. spent() alone can see a cell's cost only after it
// ends, so the first max_in_flight cells were always admitted against a total
// of ~0 — and at a width covering every cell there is no serialization point
// anywhere in the block, so the ceiling bounded nothing at all. Reserving on
// admit and settling on completion makes overshoot a function of how wrong
// the reservation is, instead of how many cells got a free pass.
type blockBudget struct {
	// usage is the block's OWN accumulator, scoped to its cells (see
	// newBlockBudget). It starts at zero, so spent() is simply its total.
	usage   *agent.RunUsage
	ceiling int
	// reserve is what one unfinished cell is assumed to cost; 0 means no
	// reservation source was found, which reduces admission to exactly the
	// spent()-only rule this had before.
	reserve int

	// mu guards reserved/inFlight, and settled broadcasts on it when a cell
	// releases its reservation. spent() reads through RunUsage, which has its
	// own lock; these are written by finishing CELL goroutines and read by the
	// admitting parent, so they need one of their own.
	mu       sync.Mutex
	settled  *sync.Cond
	reserved int
	// inFlight is how many admitted cells have not settled yet. It is what
	// tells admit whether waiting can possibly help.
	inFlight int
}

// newBlockBudget builds the block's ceiling and the context its CELLS must
// run under.
//
// The returned context carries a scoped accumulator (agent.NewScopedRunUsage)
// so spent() counts this block's own cells and nothing else. Measuring it as
// a delta on the job total instead charged the block for every agent step
// running concurrently elsewhere in the plan — an in_parallel: sibling could
// exhaust a matrix's allowance before one of its cells had spent a token.
func newBlockBudget(
	ctx context.Context, cfg *config.Config, step config.Step, cells []config.Step,
) (context.Context, *blockBudget) {
	ceiling := stepBudgetTokens(step)
	if ceiling <= 0 {
		return ctx, newIdleBlockBudget()
	}

	usage := agent.RunUsageFrom(ctx)
	if usage == nil {
		// No accumulator means no agent step can report anything, so there is
		// nothing to meter. Not an error: a matrix of tasks with a budget is
		// pointless, not wrong.
		return ctx, newIdleBlockBudget()
	}

	scoped := agent.NewScopedRunUsage(usage)

	budget := &blockBudget{
		usage: scoped, ceiling: ceiling,
		reserve: cellReserve(cfg, step, cells),
	}
	budget.settled = sync.NewCond(&budget.mu)

	return agent.WithRunUsage(ctx, scoped), budget
}

// newIdleBlockBudget is the no-ceiling budget every unbudgeted block gets:
// admit always says yes and settle does nothing. It still carries a live cond
// so the zero value can never be waited on by mistake.
func newIdleBlockBudget() *blockBudget {
	budget := &blockBudget{}
	budget.settled = sync.NewCond(&budget.mu)

	return budget
}

// soleAgentBody unwraps a cell down to the agent step inside it, when there
// is exactly one.
//
// Exactly one is the point: a block of several agents has no single ceiling
// to inherit, and guessing one of them would be worse than reserving nothing
// (which at least warns). Nested wrappers recurse, since a try: around a do:
// around an agent is a legal cell.
func soleAgentBody(cell config.Step) (config.Step, bool) {
	if cell.Agent != "" {
		return cell, true
	}

	if cell.Try != nil {
		return soleAgentBody(*cell.Try)
	}

	branches := cellBranches(cell)
	if len(branches) != 1 {
		return config.Step{}, false
	}

	return soleAgentBody(branches[0])
}

// cellBranches is the steps a container cell wraps, for soleAgentBody's walk.
func cellBranches(cell config.Step) []config.Step {
	//kindswitch:ignore only the container kinds wrap steps; the leaf kinds are the point of the default
	switch {
	case cell.Do != nil:
		return cell.Do
	case cell.InParallel != nil:
		return cell.InParallel.Steps
	case cell.Race != nil:
		return cell.Race.Steps
	case cell.Ensemble != nil:
		return cell.Ensemble.Agents
	default:
		return nil
	}
}

// cellReserve is what admission assumes one unfinished cell will spend, in
// precedence order: the block's own reserve_per_cell:, then the cell agent's
// budget.tokens (the author already declared what one invocation may cost),
// then nothing — which leaves the block admitting exactly as it did before
// reservations existed, warning included.
//
// The block's own field wins, which inverts the order issue #62 proposed. Its
// way round, an agent with a budget.tokens would silently shadow a
// reserve_per_cell: written right there on the block — config that reads like
// it binds something and does not, which is the shape this codebase rejects
// at load everywhere else. The explicit, more local declaration wins instead,
// and the common case the issue is really describing (an agent budget, no
// reserve_per_cell:) is unaffected either way.
//
// Resolved ONCE for the block rather than per cell: agent: is not among the
// fields a matrix renders per cell (see config.renderableFields), so every
// cell of a block resolves to the same agent and the same ceiling.
//
// A CLI agent yields nothing here. Its ceiling is budget.usd, which meters
// dollars inside its own subprocess and cannot be compared against a token
// allowance — so such a block needs an explicit reserve_per_cell: to bind.
func cellReserve(cfg *config.Config, step config.Step, cells []config.Step) int {
	if step.Budget != nil && step.Budget.ReservePerCell > 0 {
		return step.Budget.ReservePerCell
	}

	if len(cells) == 0 {
		return 0
	}

	// A wrapper keeps the agent on the step it wraps, not on itself, and a
	// cell body can be a try:, a do:, or a concurrent block. Unwrapping only
	// try: left every other shape with no reservation, so the documented
	// fallback to the cell agent's own budget.tokens silently did not apply.
	body, ok := soleAgentBody(cells[0])
	if !ok {
		return 0
	}

	ri, err := cfg.ResolveAgentInvocation(body)
	if err != nil {
		// An unresolvable agent is a run-time failure the cell itself will
		// report far more clearly; refusing to reserve here just leaves the
		// block admitting as it always did.
		return 0
	}

	return ri.BudgetTokens
}

// admit reports whether another cell may start, taking its reservation when
// it may — WAITING while only reservations stand in the way.
//
// The distinction is the whole correctness of this: a refusal caused by
// SPEND is permanent, because spend only grows; a refusal caused by
// RESERVATIONS is temporary, because in-flight cells release theirs as they
// finish. Treating the second as permanent truncated matrices that were
// nowhere near their ceiling — six cells costing ten tokens each against an
// allowance of 3,600 stopped after four, having spent forty.
//
// So this blocks until the reservations clear rather than giving up, and
// returns false only when spend alone has reached the ceiling (or nothing is
// in flight to release anything, which cannot happen while reserve > 0 but is
// the honest guard against waiting forever).
//
// A cell that has begun still runs to completion and keeps what it recorded:
// this bounds what STARTS, exactly as before.
func (b *blockBudget) admit() bool {
	if b.usage == nil {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for {
		if b.spent() >= b.ceiling {
			return false
		}

		if b.spent()+b.reserved < b.ceiling {
			b.reserved += b.reserve
			b.inFlight++

			return true
		}

		if b.inFlight == 0 {
			return false
		}

		b.settled.Wait()
	}
}

// settle releases a finished cell's reservation. Its real spend is already in
// the job accumulator by now — stepUsage.finish rolls it up before the
// conversation returns — so spent() picks it up with no double count. Calling
// this any earlier would open a window where neither the reservation nor the
// actual spend is counted, and a cell could be admitted against it.
func (b *blockBudget) settle() {
	if b.usage == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.reserved -= b.reserve
	b.inFlight--

	// Wakes whoever is waiting for exactly this: the allowance this cell was
	// holding is now either free again or replaced by what it really spent.
	b.settled.Broadcast()
}

// warnIfUnbindable says so when this block's width makes its own ceiling
// decorative.
//
// A reservation fixes it at any width, because admit pauses once the
// reservations fill instead of truncating: the block waits, cells finish, and
// the next admission decides on what they really spent. What cannot bind is a
// block with NO reservation source at a width covering every cell — there,
// nothing reports before the last cell is admitted and the ceiling sees a
// running total of ~0 for every decision it makes.
func (b *blockBudget) warnIfUnbindable(jobName string, maxInFlight, cells int) {
	if !b.unbindable(maxInFlight, cells) {
		return
	}

	fmt.Printf("budget: warning — max_in_flight (%d) covers all %d cells and nothing is reserved per cell, so this block's budget of %s tokens cannot stop anything\n",
		maxInFlight, cells, humanCount(b.ceiling))

	slog.Warn("across.budget.unbindable",
		"job", jobName, "max_in_flight", maxInFlight, "cells", cells,
		"budget_tokens", b.ceiling, "reserve_per_cell", b.reserve,
		"detail", "every cell is admitted before any has reported usage; set budget.reserve_per_cell (or a budget.tokens on the cell's agent) so admission pauses for real numbers, lower max_in_flight, or rely on the job budget as the backstop")
}

// unbindable reports whether this block's own ceiling can stop nothing: it
// has one, nothing is reserved on admission, and the width covers every cell,
// so no admission ever reads a nonzero total. Mirrors newLimiter's own
// "limit >= branches means no limiter at all".
//
// Any positive reservation binds at any width, because admit WAITS once the
// reservations fill rather than truncating: the block pauses, cells finish,
// and the next admission decides on real spend. Only a block with no
// reservation source at all can admit every cell blind.
func (b *blockBudget) unbindable(maxInFlight, cells int) bool {
	return b.usage != nil && b.reserve <= 0 && maxInFlight >= cells
}

func (b *blockBudget) spent() int {
	if b.usage == nil {
		return 0
	}

	return b.usage.Total()
}

// report announces that the matrix stopped early. Loudly, because a truncated
// fan-out that says nothing reads exactly like a complete one that found less:
// the cells that never ran recorded nothing, so their silence is
// indistinguishable from a clean result unless this says otherwise.
//
// It reports what admission actually decided on — spend PLUS the reservations
// still standing — not spend alone. Under max_in_flight the cells that
// consumed the allowance are typically still running and have reported
// nothing, so "spent 0 of 3,600,000" would name the one number that had no
// part in the decision and read like a stop that never should have happened.
func (b *blockBudget) report(jobName string, ran, total int) {
	spent, reserved := b.committed()

	fmt.Printf("budget: across stopped after %d of %d cells (%s of %s tokens committed: %s spent, %s reserved by cells still running)\n",
		ran, total, humanCount(spent+reserved), humanCount(b.ceiling), humanCount(spent), humanCount(reserved))

	slog.Warn("across.budget.exhausted",
		"job", jobName, "cells_run", ran, "cells_total", total,
		"spent_tokens", spent, "reserved_tokens", reserved, "budget_tokens", b.ceiling)
}

// committed is what admission weighed: reported spend, and the allowance held
// by cells that have not reported yet.
func (b *blockBudget) committed() (spent, reserved int) {
	if b.usage == nil {
		return 0, 0
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.spent(), b.reserved
}

// stopAdmitting reports whether this matrix should start no further cell:
// either the JOB's wall-clock deadline has passed (see deadlineStopsFanOut,
// which in_parallel: shares), or the block cannot afford another cell.
//
// The deadline is checked FIRST so a deadline stop takes no reservation:
// admit() consumes allowance as its way of saying yes, and a cell refused for
// time would otherwise leave a reservation behind that nothing ever settles.
func stopAdmitting(ctx context.Context, jobName string, spend *blockBudget, ran, total int) bool {
	if deadlineStopsFanOut(ctx, jobName, "across", "cells", ran, total) {
		return true
	}

	if !spend.admit() {
		spend.report(jobName, ran, total)

		return true
	}

	return false
}
