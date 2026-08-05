package config

// The budget: directive — a token ceiling on what agent work may spend.

import "fmt"

// Budget caps what an agent invocation, or a whole job's agent steps
// together, may spend. It is the AI equivalent of timeout:.
//
// Tokens only, deliberately. A token ceiling is provider-agnostic and exact.
// A money ceiling would need a per-model price table that goes stale every
// time any provider changes its rates — an ongoing maintenance burden rather
// than a one-time cost — so `cost:` is left out until someone is prepared to
// own that table.
//
// Like assert: and timeout:, a budget is an operational limit and is never
// hashed: adding one must not invalidate a cached step.
type Budget struct {
	// Tokens is the ceiling, counted from the provider's own reported usage
	// (prompt + completion, including reasoning tokens where a provider
	// reports them) — not an estimate. 0 is not "unlimited", it is rejected:
	// a budget block that caps nothing is a typo, and reading it as "no
	// limit" would be the most expensive possible interpretation.
	Tokens int `yaml:"tokens,omitempty"`
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
	}

	return nil
}

// budgetTokens is a budget's ceiling, or 0 for no budget at all.
func budgetTokens(budget *Budget) int {
	if budget == nil {
		return 0
	}

	return budget.Tokens
}

func validateBudget(label string, budget *Budget) error {
	if budget == nil {
		return nil
	}

	if budget.Tokens <= 0 {
		return fmt.Errorf("%s: budget.tokens must be a positive number of tokens (omit budget: entirely for no ceiling)", label)
	}

	return nil
}
