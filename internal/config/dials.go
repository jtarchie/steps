package config

import "fmt"

// The limit-dial convention, and the one helper that enforces it.
//
// Every dial that bounds how much an agent step may spend — max_turns:,
// max_context_bytes:, timeout: — reads the same three ways:
//
//	omitted   take the default
//	0         no limit
//	negative  load error
//
// The middle row is why these fields are pointers. A plain int cannot tell an
// author who wrote 0 from one who wrote nothing, so "no limit" was previously
// inexpressible on exactly the dials whose defaults are tuned for a bounded
// question rather than a long investigation — the complaint that opened this.
// CompactAfterTokens has needed the same distinction since it shipped; this
// generalizes its shape rather than inventing one.
//
// attempts: is deliberately OUTSIDE the convention: unbounded retry against a
// provider that is down never terminates, and the natural backstop — the
// step's deadline — is the very thing a step may now switch off. It keeps a
// floor of 1 and rejects an explicit 0 (see validateAttempts).
//
// timeout: 0 means "no deadline" on an AGENT step only. Everywhere else
// (jobs, tasks, get/put steps) omitting it already means no deadline, so a 0
// there says nothing the empty field doesn't and stays a load error — the
// same "each default matches the contract its own block already had" rule
// max_in_flight: and in_parallel:'s limit: settled between them.

// orDefault returns *set when the field was written and def when it was
// omitted. An explicit zero is a value, not an absence — that is the entire
// point of the pointer.
func orDefault[T any](set *T, def T) T {
	if set == nil {
		return def
	}

	return *set
}

// AttemptCount is how many times a step whose attempts: is set to n should
// try its work — 1 when the field was omitted. Exported because
// internal/pipeline resolves the same field for get/task/put steps, whose
// default is a single attempt rather than the agent's three.
func AttemptCount(attempts *int) int {
	return orDefault(attempts, 1)
}

// validateAgentDials checks the dials an agents: entry carries: max_turns:,
// attempts: and timeout:. The step-level spellings of the first two are
// checked where every other step field is (checkAgentOnlyFields and
// validateAttempts), since only there is the step's kind known.
func (c *Config) validateAgentDials() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		if agent.MaxTurns != nil && *agent.MaxTurns < 0 {
			return fmt.Errorf("agent %q: max_turns must not be negative (omit it for the default of %d, or set 0 for no cap)", agent.Name, defaultMaxAgentTurns)
		}

		if agent.MaxQuestions != nil && *agent.MaxQuestions < 0 {
			return fmt.Errorf("agent %q: max_questions must not be negative (omit it for the default of %d, or set 0 for no cap)", agent.Name, defaultMaxQuestions)
		}

		if agent.Timeout != "" {
			_, err := ParseTimeout(agent.Timeout)
			if err != nil {
				return fmt.Errorf("agent %q: %w", agent.Name, err)
			}
		}

		err := validateAttemptCount(agent.Attempts, fmt.Sprintf("agent %q", agent.Name))
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAttempts holds every step's attempts: to a floor of 1.
//
// The rejected value is 0, and the message has to earn it: 0 is what the
// other dials on this page use for "no limit", so an author who writes it
// here is making a reasonable guess at a convention rather than a typo. What
// they get instead is the reason it is not one.
// A fix: carries its own attempts:, and is walked explicitly here: visitSteps
// descends into try:/do:/branches/hooks, but a FixSpec is not a Step, so
// nothing else would ever reach it. That gap is how `fix: { attempts: 0 }`
// loaded cleanly and was silently reinterpreted as 1 by the retry loop's own
// floor — the exact guess this validator exists to refuse.
func (c *Config) validateAttempts() error {
	for _, task := range c.Tasks {
		if task.Fix != nil {
			err := validateAttemptCount(task.Fix.Attempts, fmt.Sprintf("task %q fix", task.Name))
			if err != nil {
				return err
			}
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			err := validateAttemptCount(step.Attempts, label)
			if err != nil {
				return err
			}

			if step.Fix != nil {
				return validateAttemptCount(step.Fix.Attempts, label+" fix")
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func validateAttemptCount(attempts *int, label string) error {
	if attempts == nil || *attempts >= 1 {
		return nil
	}

	if *attempts == 0 {
		return fmt.Errorf("%s: attempts must be at least 1 — unlike the other limits, 0 does not mean unlimited here, "+
			"because retrying a provider that is down would never stop and the deadline that would have caught it is "+
			"itself something a step can switch off (omit attempts: for the default)", label)
	}

	return fmt.Errorf("%s: attempts must not be negative (omit it for the default)", label)
}
