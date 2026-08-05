package config

// The ensemble: step — ask several agents the same question, combine their
// answers into one decision.

import (
	"fmt"
	"slices"
)

// The decision rules an ensemble can use. Anything else in decide: is read as
// the name of an agent that judges.
const (
	// DecideMajority takes the verdict more than half the voters chose.
	DecideMajority = "majority"
	// DecideUnanimous requires every voter to agree.
	DecideUnanimous = "unanimous"
	// DecideAny takes the first verdict in verdicts: that anyone chose,
	// which is the "one objection is enough" shape when the verdicts are
	// ordered from most to least severe.
	DecideAny = "any"
)

// How an ensemble treats a member that failed rather than voted.
const (
	// MemberErrorsFail fails the whole ensemble. The default: a missing vote
	// changes what the remaining votes mean, and pretending otherwise is how
	// a two-agent ensemble silently becomes a one-agent one.
	MemberErrorsFail = "fail"
	// MemberErrorsExclude decides among the members that did vote.
	MemberErrorsExclude = "exclude"
)

// Ensemble asks several agents the same question at once and combines their
// answers into one decision, which the step then routes on.
//
// A single model has blind spots. Ask one reviewer "is this correct?" and you
// get one opinion with no signal about how much to trust it; ask three and
// require a majority, and one model's bad day stops being decisive.
//
// ⚠️ N agents cost N times one. This is the step where a job budget: matters
// most.
type Ensemble struct {
	// Agents are the members, each an ordinary agent step carrying its own
	// prompt and tool selection. They vote on the block's Verdicts, which are
	// declared once here rather than repeated per member.
	Agents []Step `yaml:"agents"`
	// Verdicts is the vocabulary every member votes in, in order of
	// precedence for decide: any.
	Verdicts []string `yaml:"verdicts"`
	// Decide is the rule (majority/unanimous/any) or the name of an agent
	// that judges the members' answers.
	Decide string `yaml:"decide"`
	// MemberErrors is what to do about a member that failed rather than
	// voted: MemberErrorsFail (the default) or MemberErrorsExclude.
	MemberErrors string `yaml:"member_errors,omitempty"`
}

// FailsOnMemberError reports whether one member's failure fails the ensemble.
func (e *Ensemble) FailsOnMemberError() bool {
	return e == nil || e.MemberErrors != MemberErrorsExclude
}

// JudgeAgent returns the agent name decide: names, or "" for a rule.
func (e *Ensemble) JudgeAgent() string {
	if e == nil {
		return ""
	}

	switch e.Decide {
	case DecideMajority, DecideUnanimous, DecideAny:
		return ""
	default:
		return e.Decide
	}
}

// validateEnsemble enforces the shape of an ensemble: block.
func (c *Config) validateEnsemble() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Ensemble == nil {
				return nil
			}

			return c.validateEnsembleBlock(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateEnsembleBlock(label string, step *Step) error {
	ensemble := step.Ensemble

	if len(ensemble.Agents) < 2 {
		return fmt.Errorf("%s: ensemble.agents needs at least two members; one opinion is a plain agent step", label)
	}

	if len(ensemble.Verdicts) == 0 {
		return fmt.Errorf("%s: ensemble.verdicts must list the vocabulary the members vote in", label)
	}

	err := c.validateEnsembleMembers(label, ensemble)
	if err != nil {
		return err
	}

	if ensemble.MemberErrors != "" &&
		ensemble.MemberErrors != MemberErrorsFail && ensemble.MemberErrors != MemberErrorsExclude {
		return fmt.Errorf("%s: ensemble.member_errors %q is not valid; use %q or %q%s",
			label, ensemble.MemberErrors, MemberErrorsFail, MemberErrorsExclude,
			suggestion(ensemble.MemberErrors, []string{MemberErrorsFail, MemberErrorsExclude}))
	}

	return c.validateEnsembleDecision(label, ensemble)
}

// validateEnsembleMembers checks each member is an agent step that leaves the
// block's own concerns — the verdict vocabulary and the routing — to the block.
func (c *Config) validateEnsembleMembers(label string, ensemble *Ensemble) error {
	for i := range ensemble.Agents {
		member := &ensemble.Agents[i]

		if member.Agent == "" {
			return fmt.Errorf("%s: ensemble.agents[%d] must be an agent step; an ensemble combines model opinions, and a task has none", label, i)
		}

		if len(member.Verdicts) > 0 {
			return fmt.Errorf("%s: ensemble.agents[%d]: verdicts belong on the ensemble, not on a member — every member votes in the same vocabulary", label, i)
		}

		if member.To != nil {
			return fmt.Errorf("%s: ensemble.agents[%d]: to: belongs on the ensemble, which routes on the DECISION; a member routing on its own vote would leave the block half-taken", label, i)
		}
	}

	return nil
}

// validateEnsembleDecision checks decide: names a rule or a real agent, and
// that a rule that can tie has something to break the tie with.
func (c *Config) validateEnsembleDecision(label string, ensemble *Ensemble) error {
	judge := ensemble.JudgeAgent()
	if judge == "" {
		if ensemble.Decide == "" {
			return fmt.Errorf("%s: ensemble.decide is required; use %q, %q, %q, or the name of an agent to judge",
				label, DecideMajority, DecideUnanimous, DecideAny)
		}

		return nil
	}

	_, err := c.FindAgent(judge)
	if err != nil {
		return fmt.Errorf("%s: ensemble.decide %q is neither a decision rule (%s, %s, %s) nor a known agent: %w",
			label, ensemble.Decide, DecideMajority, DecideUnanimous, DecideAny, err)
	}

	// A judge is itself a member of nothing: it must not also be voting, or
	// it would be marking its own homework.
	for i := range ensemble.Agents {
		if ensemble.Agents[i].Agent == judge {
			return fmt.Errorf("%s: ensemble.decide names %q, which is also a voting member; a judge must not mark its own vote", label, judge)
		}
	}

	return nil
}

// EnsembleVerdictsFor returns a copy of the vocabulary an ensemble votes in.
func (e *Ensemble) EnsembleVerdictsFor() []string {
	return slices.Clone(e.Verdicts)
}
