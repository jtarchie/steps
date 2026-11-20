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

// validateStepSandbox holds the step's spelling to what it can mean: true on a task or agent step, whose commands it fences, or on a block, whose guard and contents it fences (fenceBlocks). False would read as unfencing the entry or the block's contents, which a step cannot do; a get or put runs its type's commands, which the type fences, and a load_var or approval has none.
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
			case StepKindTask, StepKindAgent, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindDo:
				return nil
			case StepKindGet, StepKindPut, StepKindLoadVar, StepKindApproval, StepKindAsk:
			}

			return fmt.Errorf("%s: sandbox: fences a task's, agent's or block's commands, and this step runs none; set it on the type, or on a block around the step", label)
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

// resolvedStepSandbox is whether a step runs fenced: its own sandbox: true, the tasks: or agents: entry it references, a get/put's resource type, or a block's own.
func (c *Config) resolvedStepSandbox(step Step) bool {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		return step.Sandboxed() || c.entrySandbox(step)
	case StepKindAgent:
		agent, _ := c.FindAgent(step.Agent)

		return step.Sandboxed() || (agent != nil && agent.Sandbox)
	case StepKindGet, StepKindPut:
		return c.typeSandbox(step)
	case StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindDo:
		return step.Sandboxed()
	case StepKindLoadVar, StepKindApproval, StepKindAsk:
	}

	return false
}

// typeSandbox is whether a get or put's resource type asked for the fence.
func (c *Config) typeSandbox(step Step) bool {
	name, _ := step.resourceName()

	resource, _ := c.FindResource(name)
	if resource == nil {
		return false
	}

	rt, _ := c.FindResourceType(resource.Type)

	return rt != nil && rt.Sandbox
}

// fenceBlocks stamps a block's sandbox: true onto everything inside it that runs here, after validation and resolveTags, so every reader (ResolveTask, ResolveAgentInvocation, a guard's runtime, the fence lists) sees one field. A step in an image or placed on a worker is left as it is; that is the add-only fence never reaching where it cannot hold.
func (c *Config) fenceBlocks() {
	for _, job := range c.Jobs {
		_ = walkJob(job, false, handDownBlockFence, func(_ string, step *Step, fenced bool) error {
			if fenced && len(step.Tags) == 0 && c.runsHere(*step) {
				on := true
				step.Sandbox = &on
			}

			return nil
		})
	}
}

// handDownBlockFence: a fenced block fences its children and its hooks, and whatever is inside a fenced block passes the fence on.
func handDownBlockFence(_ string, step *Step, fenced bool) (bool, bool) {
	kind, _ := step.Kind()
	fenced = fenced || (kind != StepKindTask && kind != StepKindAgent && step.Sandboxed())

	return fenced, fenced
}

// runsHere is whether a step's commands, or its when: guard, run on this machine outside an image: what a block's fence can reach.
func (c *Config) runsHere(step Step) bool {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask, StepKindAgent:
		return c.resolvedStepImage(step) == ""
	case StepKindPut:
		resource, err := c.FindResource(step.PutResourceName())
		if err != nil {
			return false
		}

		rt, _ := c.FindResourceType(resource.Type)

		return rt != nil && rt.Image == "" && step.Guarded()
	case StepKindLoadVar, StepKindApproval, StepKindAsk:
		return step.Guarded()
	case StepKindGet:
	case StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindDo:
		return true
	}

	return false
}
