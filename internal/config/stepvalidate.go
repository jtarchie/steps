package config

// Load-time checks on a plan step: does it name things that exist, and are
// its fields on a kind that can honour them?

import (
	"fmt"
)

// resolveGetReference resolves a get step's resource, unless it aliases one —
// an alias is validateGetResource's to check.
func (c *Config) resolveGetReference(step *Step) error {
	if step.Resource != "" {
		return nil
	}

	_, err := c.FindResource(step.Get)

	return err
}

// resolveBranchReferences resolves every branch of an in_parallel: block. A
// block references nothing itself; its branches are ordinary steps and
// reference exactly what they always did.
func (c *Config) resolveBranchReferences(steps []Step) error {
	for i := range steps {
		err := c.resolveStepReference(&steps[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// validateStepReferences rejects a step naming a resource, agent, or tasks:
// entry that the pipeline does not define.
//
// A misspelled name is the most common way to break an otherwise well-formed
// pipeline, and it used to survive load: only a get step's resource: alias was
// checked, so `agent: reviwer` or `put: relase` failed partway through a run,
// after earlier steps had already done their work. Checking every reference up
// front makes a typo cost a load rather than a build.
func (c *Config) validateStepReferences() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			err := c.resolveStepReference(step)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// resolveStepReference looks up whatever a step names, reporting the lookup
// error unchanged. A step whose kind is malformed resolves to nothing —
// validateStepKinds reports that on its own rather than having every caller
// repeat it.
func (c *Config) resolveStepReference(step *Step) error {
	// A load_var: names a file, not a pipeline entity, so there is nothing to
	// resolve against resources, tasks, or agents — same as a malformed step,
	// whose kind another validator reports.
	// A load_var: names a file and an approval: names a person; neither
	// references a pipeline entity, same as a malformed step whose kind
	// another validator reports.
	kind, ok := step.Kind()
	if !ok || !kind.ReferencesEntities() {
		return nil
	}

	var err error

	switch kind { //nolint:exhaustive // StepKindLoadVar returned above
	case StepKindGet:
		return c.resolveGetReference(step)
	case StepKindPut:
		_, err = c.FindResource(step.Put)
	case StepKindAgent:
		_, err = c.FindAgent(step.Agent)
	case StepKindTry:
		err = c.resolveStepReference(step.Try)
	case StepKindInParallel, StepKindRace, StepKindEnsemble:
		for _, branches := range branchesOf(step) {
			err = c.resolveBranchReferences(branches)
		}
	case StepKindTask:
		return c.resolveTaskReference(step)
	}

	return err
}

// resolveTaskReference resolves a task step's tasks: entry. An inline task —
// one carrying its own run: — never consults tasks: at all.
func (c *Config) resolveTaskReference(step *Step) error {
	if step.Run != "" {
		return nil
	}

	_, err := c.FindTask(step.Task)

	return err
}

// validateStepFieldPlacement rejects the three kind-specific fields that had
// no placement check: trigger: and version: (get-only) and params:
// (get/put-only).
//
// Every other kind-specific field already errors when written on the wrong
// kind. These three were silently ignored instead, so `trigger: true` on a
// task step — a plausible reading of "run this when something changes" —
// looked accepted and did nothing.
//
// params: is valid on BOTH resource-facing kinds, matching Concourse: a put's
// params reach out:, a get's reach in: (concourse-ci.org/docs/steps/get/).
// A get's params are how a resource is told HOW to fetch — git's depth: and
// submodules:, s3's unpack: — which is per-get rather than per-resource, and
// so cannot live in source: without forcing two resources for two fetch
// styles of one repository.
func (c *Config) validateStepFieldPlacement() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(checkStepFieldPlacement)
		if err != nil {
			return err
		}
	}

	return nil
}

// misplacedField is one rule: a condition that means the field was written
// on a kind that cannot honour it, and the sentence saying where it belongs.
type misplacedField struct {
	misplaced bool
	why       string
}

// firstMisplaced reports the first rule that fired, labelled.
//
// A table rather than a chain of switches: each rule is one line, in the
// order they are reported, and adding one cannot push its function over the
// complexity budget the way another link in a chain of default-cases would.
func firstMisplaced(label string, rules []misplacedField) error {
	for _, rule := range rules {
		if rule.misplaced {
			return fmt.Errorf("%s: %s", label, rule.why)
		}
	}

	return nil
}

// checkStepFieldPlacement is validateStepFieldPlacement's per-step half: a
// field that only means something on one kind, written on another, is a
// mistake rather than a no-op.
func checkStepFieldPlacement(label string, step *Step) error {
	err := checkResourceOnlyFields(label, step)
	if err != nil {
		return err
	}

	return checkAgentOnlyFields(label, step)
}

// checkResourceOnlyFields places the fields that only a get or a put can act
// on. params: is valid on BOTH, matching Concourse: a put's params reach out:,
// a get's reach in: (concourse-ci.org/docs/steps/get/). A get's params are how
// a resource is told HOW to fetch — git's depth: and submodules:, s3's unpack:
// — which is per-get rather than per-resource, and so cannot live in source:
// without forcing two resources for two fetch styles of one repository.
func checkResourceOnlyFields(label string, step *Step) error {
	isGet, isPut := step.Get != "", step.Put != ""

	return firstMisplaced(label, []misplacedField{
		{step.Trigger && !isGet, "trigger is only valid on get steps"},
		{step.Version != nil && !isGet, "version is only valid on get steps"},
		{step.Params != nil && !isGet && !isPut, "params is only valid on get and put steps"},
	})
}

// checkAgentOnlyFields rejects the agent-conversation fields on any other step
// kind. Each of these previously parsed cleanly anywhere and was silently
// ignored off an agent step — the pipeline read as if a prompt or a tool
// selection were in force while nothing consumed it, the exact failure
// placement validation exists to prevent.
func checkAgentOnlyFields(label string, step *Step) error {
	isAgent := step.Agent != ""

	return firstMisplaced(label, []misplacedField{
		{step.MaxTurns != nil && *step.MaxTurns < 0, "max_turns must not be negative (omit it to take the agent's, or set 0 for no cap)"},
		{step.MaxTurns != nil && !isAgent, "max_turns is only valid on agent steps (it bounds the tool-calling loop; a task has no turns)"},
		{step.MaxQuestions != nil && *step.MaxQuestions < 0, "max_questions must not be negative (omit it to take the agent's, or set 0 for no cap)"},
		{step.MaxQuestions != nil && !isAgent, "max_questions is only valid on agent steps (it bounds ask_user calls; nothing else can ask)"},
		{len(step.Messages) > 0 && !isAgent, "messages is only valid on agent steps (nothing else holds a conversation)"},
		{len(step.MessageFiles) > 0 && !isAgent, "message_files is only valid on agent steps (nothing else holds a conversation)"},
		{step.Dir != "" && !isAgent, "dir is only valid on agent steps (a task embeds a cd in its run:)"},
		{len(step.Tools) > 0 && !isAgent, "tools is only valid on agent steps (it selects from the agent's grant; a task's fix: carries its own tools)"},
	})
}

// validateTrySteps rejects malformed try wrappers: a bare try: (nil inner
// step), try: wrapped around a get step, try: that also sets another kind
// field, or a try: whose inner step has no recognized kind. It also enforces
// the division of fields between the wrapper and what it wraps — see
// validateWrappedStepFields.
func (c *Config) validateTrySteps() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Try == nil {
				return nil
			}

			innerKind, ok := step.Try.Kind()
			if !ok {
				return fmt.Errorf("%s: try: wraps an unrecognized or empty step", label)
			}
			if innerKind == StepKindGet {
				return fmt.Errorf("%s: try: cannot wrap a get step", label)
			}

			fields := step.kindFieldsSet()
			if len(fields) > 1 { // "try" + something else
				return fmt.Errorf("%s: try: is a wrapper — do not also set get/task/put/agent", label)
			}

			return validateWrappedStepFields(label, step.Try)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateWrappedStepFields rejects the fields that are dead config on the step
// inside a try:, because only the wrapper occupies a position in the plan.
//
// Routing is resolved against the plan step (internal/pipeline's applyRouting
// never sees the wrapped step), so a to:/max_visits: written one level too deep
// used to load fine and then silently never fire — the plan just fell through
// past a failure the author believed they had routed.
func validateWrappedStepFields(label string, inner *Step) error {
	switch {
	case inner.To != nil:
		return fmt.Errorf("%s: to: belongs on the try: step, not the step it wraps (only the wrapper has a position in the plan)", label)
	case inner.MaxVisits != 0:
		return fmt.Errorf("%s: max_visits belongs on the try: step, not the step it wraps (only the wrapper has a position in the plan)", label)
	default:
		return nil
	}
}
