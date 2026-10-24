package config

import "fmt"

// validateSandbox refuses every sandbox: the fence could not keep. It fences a process on THIS machine, so a container (already its own fence, and one sandbox-exec cannot reach into) and a placed step (running on another machine) are both load errors rather than a quiet no-op. The OS check is not here: the doc corpus validates on every OS, so an unsupported one refuses at the step instead, as strategy: btrfs does.
func (c *Config) validateSandbox() error {
	err := c.visitContainerSettings(func(context string, settings containerSettings) error {
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

// resolvedStepSandbox is whether the entry a step runs under asked for the fence: the tasks: or agents: entry it references, or a get/put's resource type. An inline run: names no entry, so it has none.
func (c *Config) resolvedStepSandbox(step Step) bool {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		if step.Run != "" {
			return false
		}

		task, _ := c.FindTask(step.Task)

		return task != nil && task.Sandbox
	case StepKindAgent:
		agent, _ := c.FindAgent(step.Agent)

		return agent != nil && agent.Sandbox
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
