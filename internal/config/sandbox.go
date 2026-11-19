package config

import "fmt"

// validateSandbox refuses every sandbox: the fence could not keep. It fences a process on THIS machine, so a container (already its own fence, and one sandbox-exec cannot reach into) and a placed step (running on another machine) are both load errors rather than a quiet no-op. The OS check is not here: the doc corpus validates on every OS, so an unsupported one refuses at the step instead.
func (c *Config) validateSandbox() error {
	err := c.validateStepSandbox()
	if err != nil {
		return err
	}

	err = c.visitContainerSettings(func(context string, settings containerSettings) error {
		if settings.Sandbox && settings.Image != "" {
			return fmt.Errorf("%s: sandbox fences host commands, and image: %q runs them in a container — drop one", context, settings.Image)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return c.visitPlacements(func(label string, step *Step, placed placement) error {
		if len(placed.tags) > 0 && c.resolvedStepSandbox(*step) {
			return fmt.Errorf("%s: sandbox fences commands on this machine, and tags: %v places this step on a worker", label, placed.tags)
		}

		return nil
	})
}

// Sandboxed is whether the step itself asked for the fence.
func (s Step) Sandboxed() bool {
	return s.Sandbox != nil && *s.Sandbox
}

// validateStepSandbox holds the step's spelling to what it can mean: true on a task or agent step, whose commands it fences. False would read as unfencing the entry, which a step cannot do, and every other kind has no command of its own (a get or put runs its type's, which the type fences).
func (c *Config) validateStepSandbox() error {
	err := c.rejectOnGetAndPut("sandbox", func(s *Step) bool { return s.Sandbox != nil })
	if err != nil {
		return err
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Sandbox == nil {
				return nil
			}

			if !*step.Sandbox {
				return fmt.Errorf("%s: sandbox: false on a step cannot unfence anything — a step's sandbox: only adds the fence; drop it", label)
			}

			switch kind, _ := step.Kind(); kind {
			case StepKindTask, StepKindAgent:
				return nil
			case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindDo:
			}

			return fmt.Errorf("%s: sandbox: fences a task's or agent's own commands, and this step runs none; set it on the steps inside", label)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// entrySandbox is whether a task step's tasks: entry asked for the fence; an inline run: has no entry.
func (c *Config) entrySandbox(step Step) bool {
	task := c.taskEntry(step)

	return task != nil && task.Sandbox
}

// resolvedStepSandbox is whether a step runs fenced: its own sandbox: true, the tasks: or agents: entry it references, or a get/put's resource type.
func (c *Config) resolvedStepSandbox(step Step) bool {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		return step.Sandboxed() || c.entrySandbox(step)
	case StepKindAgent:
		agent, _ := c.FindAgent(step.Agent)

		return step.Sandboxed() || (agent != nil && agent.Sandbox)
	case StepKindGet, StepKindPut:
		name, _ := step.resourceName()

		resource, _ := c.FindResource(name)
		if resource == nil {
			return false
		}

		rt, _ := c.FindResourceType(resource.Type)

		return rt != nil && rt.Sandbox
	case StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindDo:
	}

	return false
}
