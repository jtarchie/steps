package config

// The tags: field: which machine a step's commands run on.
//
// A tag names a capability the step needs, and the invocation maps that name to
// a machine (see --worker). The split is deliberate and matches Concourse's:
// a pipeline that named hosts would stop being portable the moment anyone else
// ran it.

import (
	"fmt"
	"strings"
)

// validateTagRules groups the tags:-related load-time checks, mirroring
// validateNetworkRules.
func (c *Config) validateTagRules() error {
	err := c.validateTagValues()
	if err != nil {
		return err
	}

	// Placement first, so tags: on a put reports the reason that actually
	// helps rather than one of the narrower rules tripping on a step whose
	// image this walk resolves differently.
	err = c.rejectOnGetAndPut("tags", func(s *Step) bool { return len(s.Tags) > 0 })
	if err != nil {
		return err
	}

	return c.validateTagsRejectAgent()
}

// validateTagValues rejects a tag that cannot name anything.
//
// One tag, not several: Concourse intersects a step's tags against a pool of
// workers advertising their own, and there is no pool here — an invocation
// maps one name to one machine. Two tags would be two machines and the step
// would have no home, so the ambiguity is refused rather than resolved by a
// rule nobody would guess.
func (c *Config) validateTagValues() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Tags == nil {
				return nil
			}

			if len(step.Tags) == 0 {
				return fmt.Errorf("%s: tags: is empty — remove it, or name the worker the step needs", label)
			}

			if len(step.Tags) > 1 {
				return fmt.Errorf("%s: tags: names %d workers (%s); a step runs on one machine, so name one",
					label, len(step.Tags), strings.Join(step.Tags, ", "))
			}

			if strings.TrimSpace(step.Tags[0]) == "" {
				return fmt.Errorf("%s: tags: has a blank entry", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateTagsRejectAgent refuses tags: on an agent step.
//
// An agent step is a conversation the orchestrator holds: the model, the tool
// calls, the MCP servers and the file tools all live in this process, reading
// the step's directory here. Only its run_shell would travel, which would put
// half a step on each machine — a model reading one filesystem and writing to
// another, with no way to tell. Refused until an agent can be placed whole.
func (c *Config) validateTagsRejectAgent() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.Tags) > 0 && step.Agent != "" {
				return fmt.Errorf("%s (agent %q): tags: is not valid on an agent step — its tools and conversation run here, so only its shell commands would move, leaving half the step on each machine",
					label, step.Agent)
			}

			// Same split, reached a different way: a task's fix: builds an
			// agent from THIS step, with its file tools on the local directory
			// and its run_shell on the runner the task uses. The agent rule
			// above does not see it, because the step is a task.
			//
			// The RESOLVED fix:, for the reason validateTagsRejectImage reads
			// the resolved image: a step that names a tasks: entry inherits
			// that entry's fix: (see ResolveTask), so checking only the step's
			// own let exactly this split through — the command ran on the
			// worker while the repair agent read the local workspace.
			if len(step.Tags) > 0 && c.resolvedStepFix(*step) != nil {
				return fmt.Errorf("%s: tags: is not valid on a task with fix: — the repair agent reads this machine's copy of the step while its commands would run on the worker",
					label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// resolvedStepFix reports the fix: a task step would actually run with,
// merging the step's own over the tasks: entry it references. It answers nil
// for anything it cannot resolve, leaving that error to whichever validator
// owns it.
func (c *Config) resolvedStepFix(step Step) *FixSpec {
	if step.Fix != nil {
		return step.Fix
	}

	if step.Task == "" {
		return nil
	}

	task, err := c.FindTask(step.Task)
	if err != nil {
		return nil
	}

	return task.Fix
}
