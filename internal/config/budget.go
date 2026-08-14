package config

// The budget: directive — a token ceiling on what agent work may spend.

import "fmt"

// Budget caps what an agent invocation, or a whole job's agent steps
// together, may spend. It is the AI equivalent of timeout:.
//
// Two spellings, because the two runners meter different things and neither
// can be converted into the other honestly. A hosted agent's conversation is
// driven here, so tokens are counted exactly as the provider reports them; a
// CLI agent's conversation happens inside a subprocess that meters itself in
// dollars. Translating between them would need a per-model price table that
// goes stale every time any provider changes its rates — an ongoing
// maintenance burden, and a number that would silently go wrong — so each
// runner takes the unit it can actually enforce and validation rejects the
// other (see validateCLIAgents).
//
// Like assert: and timeout:, a budget is an operational limit and is never
// hashed: adding one must not invalidate a cached step.
type Budget struct {
	// Tokens is the ceiling for a HOSTED agent, counted from the provider's
	// own reported usage (prompt + completion, including reasoning tokens
	// where a provider reports them) — not an estimate. 0 is not "unlimited",
	// it is rejected: a budget block that caps nothing is a typo, and reading
	// it as "no limit" would be the most expensive possible interpretation.
	Tokens int `yaml:"tokens,omitempty"`
	// USD is the ceiling for a CLI agent, enforced by the CLI itself
	// mid-conversation. It exists because a subprocess reports its spend only
	// on exit — too late for a token counter here to stop anything — while the
	// CLI has a real circuit breaker of its own that takes dollars.
	USD float64 `yaml:"usd,omitempty"`
	// ReservePerCell is what an across: block's admission control ASSUMES one
	// not-yet-finished cell will spend, so Tokens binds at any max_in_flight.
	//
	// Admission can only see what FINISHED cells reported, so without a
	// reservation the first max_in_flight cells are always admitted against a
	// total of ~0 — and when the width covers every cell there is no
	// serialization point at all and the ceiling bounds nothing. Reserving on
	// admit and settling on completion closes that: overshoot becomes bounded
	// by how wrong the reservation is rather than by how many cells got a free
	// pass.
	//
	// Across-only, like Tokens on a step. Unset falls back to the cell agent's
	// own budget.tokens (the author already declared what one invocation may
	// cost), and with neither the block behaves exactly as it did before this
	// existed. Deliberately no global default: a guessed reservation that
	// silently under-admits is worse than an honest warning.
	ReservePerCell int `yaml:"reserve_per_cell,omitempty"`
}

// set reports whether this budget caps anything at all.
func (b *Budget) set() bool {
	return b != nil && (b.Tokens != 0 || b.USD != 0)
}

// validateBudgets rejects a budget: block that caps nothing, on an agent or a
// job. A negative ceiling is rejected for the same reason: it would trip on
// the first turn, which nobody means.
func (c *Config) validateBudgets() error {
	for _, agent := range c.Agents {
		label := fmt.Sprintf("agent %q", agent.Name)

		err := validateBudget(label, agent.Budget)
		if err != nil {
			return err
		}

		err = rejectReservePerCell(label, agent.Budget)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		label := fmt.Sprintf("job %q", job.Name)

		err := validateBudget(label, job.Budget)
		if err != nil {
			return err
		}

		err = rejectReservePerCell(label, job.Budget)
		if err != nil {
			return err
		}

		if job.MaxConsecutiveFailures < 0 {
			return fmt.Errorf("job %q: max_consecutive_failures must not be negative (omit it for no circuit breaker)", job.Name)
		}

		err = validateStepBudgets(job)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateStepBudgets checks the block-scoped budget: an across: step may cap
// what its cells spend together.
//
// Only there, and only in tokens. Not on an ordinary step, because an agent
// step already has its own ceiling and a task spends nothing — a budget there
// would be a field that reads like it does something and does not. Not in
// dollars, because USD is a ceiling only a CLI agent's own subprocess can
// enforce (see validateCLIAgents), and it enforces it against ITSELF: a
// subprocess cannot know what the other cells of a matrix have already spent,
// so a dollar budget on a block would silently cap nothing.
func validateStepBudgets(job Job) error {
	return job.visitSteps(func(label string, step *Step) error {
		if step.Budget == nil {
			return nil
		}

		if len(step.Across) == 0 {
			return fmt.Errorf("%s: budget is only valid on an across: step, where it caps what the cells spend together; an agent's own ceiling goes on the agent, and a whole job's on the job", label)
		}

		err := validateBudget(label, step.Budget)
		if err != nil {
			return err
		}

		if step.Budget.USD != 0 {
			return fmt.Errorf("%s: budget.usd is not valid on an across: step — a dollar ceiling is enforced inside a CLI agent's own subprocess, which cannot see what the matrix's other cells have spent; use budget.tokens", label)
		}

		if step.Budget.ReservePerCell < 0 {
			return fmt.Errorf("%s: budget.reserve_per_cell must be a positive number of tokens (omit it to reserve the cell agent's own budget.tokens, or nothing)", label)
		}

		// A reservation only decides anything when a cell must be admitted
		// while another is still running and has reported nothing. The serial
		// walk settles each cell before admitting the next, so the reservation
		// is always zero at the check and admission is the plain spent() rule
		// it was before reservations existed — accurate already, because
		// serial always knows the real spend.
		//
		// Rejected rather than ignored for the same reason the field is
		// rejected on an agent and a job: config that reads like a spending
		// control and binds nothing is the failure mode worth failing loudly.
		if step.Budget.ReservePerCell > 0 && step.MaxInFlight <= 1 {
			return fmt.Errorf("%s: budget.reserve_per_cell needs max_in_flight: above 1 — cells run one at a time here, so each one's real spend is known before the next is admitted and a reservation would decide nothing", label)
		}

		return nil
	})
}

// rejectReservePerCell refuses reserve_per_cell: anywhere but an across:
// block's budget.
//
// Budget is one type shared by agents, jobs and across: steps, so the field
// parses in all three positions; only the block has an admission decision for
// it to inform. An agent's budget caps one invocation and a job's is a
// backstop — neither admits anything — so a reservation there would read like
// configuration and bind nothing, which is the failure mode this codebase
// rejects at load everywhere else.
func rejectReservePerCell(label string, budget *Budget) error {
	if budget == nil || budget.ReservePerCell == 0 {
		return nil
	}

	return fmt.Errorf("%s: budget.reserve_per_cell is only valid on an across: step, where it is what admission assumes an unfinished cell will spend", label)
}

// budgetTokens is a budget's token ceiling, or 0 for no token budget.
func budgetTokens(budget *Budget) int {
	if budget == nil {
		return 0
	}

	return budget.Tokens
}

// budgetUSD is a budget's dollar ceiling, or 0 for no dollar budget.
func budgetUSD(budget *Budget) float64 {
	if budget == nil {
		return 0
	}

	return budget.USD
}

// validateBudget rejects a budget: block that caps nothing. Which SPELLING is
// allowed where is a separate question, answered by the runner that would
// enforce it (validateCLIAgents); this only insists that whatever was written
// is a real ceiling.
func validateBudget(label string, budget *Budget) error {
	if budget == nil {
		return nil
	}

	if !budget.set() {
		return fmt.Errorf("%s: budget must set tokens or usd to a positive ceiling (omit budget: entirely for no ceiling)", label)
	}

	if budget.Tokens < 0 {
		return fmt.Errorf("%s: budget.tokens must be a positive number of tokens (omit budget: entirely for no ceiling)", label)
	}

	if budget.USD < 0 {
		return fmt.Errorf("%s: budget.usd must be a positive dollar amount (omit budget: entirely for no ceiling)", label)
	}

	return nil
}
