package config

// The tags: field: which machine a step's, or a resource's, commands run on.
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

	err = c.validateTagsRejectTry()
	if err != nil {
		return err
	}

	err = c.validateTagsRejectAgent()
	if err != nil {
		return err
	}

	return c.validateTagsRejectNonShell()
}

// validateTagValues rejects a tag that cannot name anything, on a step or on
// a resource.
func (c *Config) validateTagValues() error {
	for _, resource := range c.Resources {
		err := validateTagList("resource "+resource.Name, resource.Tags)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return validateTagList(label, step.Tags)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateTagList is the shape of one tags: list, wherever it appears.
//
// One tag, not several: Concourse intersects a step's tags against a pool of
// workers advertising their own, and there is no pool here — an invocation
// maps one name to one machine. Two tags would be two machines and the step
// would have no home, so the ambiguity is refused rather than resolved by a
// rule nobody would guess.
func validateTagList(label string, tags []string) error {
	if tags == nil {
		return nil
	}

	if len(tags) == 0 {
		return fmt.Errorf("%s: tags: is empty — remove it, or name the worker the step needs", label)
	}

	if len(tags) > 1 {
		return fmt.Errorf("%s: tags: names %d workers (%s); a step runs on one machine, so name one",
			label, len(tags), strings.Join(tags, ", "))
	}

	if strings.TrimSpace(tags[0]) == "" {
		return fmt.Errorf("%s: tags: has a blank entry", label)
	}

	return nil
}

// validateTagsRejectTry refuses tags: on a try: wrapper, for the reason every
// execution setting is: the value would be accepted and then ignored, since
// resolution reads the wrapped step, never the wrapper.
func (c *Config) validateTagsRejectTry() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Try != nil && len(step.Tags) > 0 {
				return fmt.Errorf("%s: tags is not valid on a try: step; set it on the step try: wraps", label)
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
			// The RESOLVED fix:, for the same reason the agent rule above reads
			// a resolved step: a step that names a tasks: entry inherits
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

// validateTagsRejectNonShell refuses placing a resource whose type is not
// shell-backed.
//
// The same split as an agent: an mcp: type's tool call and an expr: type's
// program both run in this process and write the fetched files with their own
// hands, so a tag could only move a fraction of the stage. Only a shell
// type's check/in/out are commands a worker can run whole.
func (c *Config) validateTagsRejectNonShell() error {
	for _, resource := range c.Resources {
		if len(resource.Tags) == 0 {
			continue
		}

		err := c.rejectNonShellPlacement("resource "+resource.Name, resource.Type)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			name, ok := step.resourceName()
			if !ok || len(step.Tags) == 0 {
				return nil
			}

			resource, err := c.FindResource(name)
			if err != nil {
				return nil //nolint:nilerr // reported by the rule that owns unknown resources
			}

			return c.rejectNonShellPlacement(label, resource.Type)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// resourceName is the resource a get or put step names, and false for every
// other kind.
func (s Step) resourceName() (string, bool) {
	//kindswitch:ignore only a get or a put names a resource; the other kinds are the false answer
	switch {
	case s.Get != "":
		return s.GetResourceName(), true
	case s.Put != "":
		return s.Put, true
	default:
		return "", false
	}
}

func (c *Config) rejectNonShellPlacement(label, typeName string) error {
	resourceType, err := c.FindResourceType(typeName)
	if err != nil {
		return nil //nolint:nilerr // reported by the rule that owns unknown types
	}

	backend := resourceType.Config.Backend()
	if backend == BackendShell {
		return nil
	}

	return fmt.Errorf("%s: tags: is not valid on a resource of type %q — its %s in/out run inside this process, so only their files could move; make the type shell-backed to place it",
		label, typeName, backend)
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

// inheritResourceTags gives every untagged get and put its resource's tags:,
// after validation, so everything downstream reads one field.
//
// Resolved at load rather than at each reader, the way ResolveTask merges a
// step over its tasks: entry: the readers are many (which machine to dial,
// what the run record says, whether an image is pulled here, whether a job
// can be placed at all) and a fallback missed in one of them is a step
// quietly running on the wrong machine. Tags are not hashed, so this changes
// nothing about what a step produces.
func (c *Config) inheritResourceTags() {
	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			name, ok := step.resourceName()
			if !ok || len(step.Tags) > 0 {
				return nil
			}

			resource, err := c.FindResource(name)
			if err == nil && len(resource.Tags) > 0 {
				step.Tags = append([]string(nil), resource.Tags...)
			}

			return nil
		})
	}
}
