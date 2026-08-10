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
		err := validateBudget(fmt.Sprintf("agent %q", agent.Name), agent.Budget)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := validateBudget(fmt.Sprintf("job %q", job.Name), job.Budget)
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

		return nil
	})
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
