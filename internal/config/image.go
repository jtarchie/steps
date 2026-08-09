package config

// The image: field wherever it appears, and whether a pipeline needs a
// container runtime at all.

import (
	"fmt"
	"slices"
	"strings"
)

// validateImageRules groups the three image:-related load-time checks
// (grouped into one call so config.validate's own branch count doesn't grow
// with every image: rule added — see cyclop): image: is invalid on get/put
// steps, an image: value must not look like a docker flag, and a fix: agent
// may not set its own image:.
func (c *Config) validateImageRules() error {
	err := c.validateImages()
	if err != nil {
		return err
	}

	err = c.validateImageValues()
	if err != nil {
		return err
	}

	return c.validateFixAgentImages()
}

// validateImages rejects image: on get/put steps: a put's execution image
// comes from its resource type (ResourceType.Image), and a get step has no
// task/agent to scope.
func (c *Config) validateImages() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Image == "" {
				return nil
			}

			//kindswitch:ignore Task and Agent are the kinds image: is FOR — the cases here are the rejections
			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): image is not valid on get steps", label, step.Get)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): image is not valid on put steps; set it on the resource_type instead", label, step.Put)
			case step.Try != nil:
				// A wrapper's own image: was accepted and then ignored:
				// resolveStepImage recurses into step.Try and reads the
				// wrapped step's image, never the wrapper's. Silently doing
				// nothing is worse than refusing.
				return fmt.Errorf("%s: image is not valid on a try: step; set it on the step try: wraps", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateImageValues rejects an image: value that could be misread as a
// docker flag rather than an image reference: anything starting with '-'
// (e.g. "--privileged", "-v", "--network=host"). shell.dockerRunArgs also
// inserts a literal "--" before the image argument as defense in depth, but
// this check is what turns a mistyped or supply-chain-tainted image string
// into a clear LoadConfig error instead of docker silently granting whatever
// the flag means (privileged mode, an arbitrary bind mount, host
// networking). Checked wherever image: can be set: resource_types, agents,
// tasks, and steps (a step's own image: override).
func (c *Config) validateImageValues() error {
	for i := range c.ResourceTypes {
		rt := c.ResourceTypes[i]

		err := checkImageValue(fmt.Sprintf("resource_type %q", rt.Name), rt.Image)
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkImageValue(fmt.Sprintf("agent %q", agent.Name), agent.Image)
		if err != nil {
			return err
		}
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := checkImageValue(fmt.Sprintf("task %q", task.Name), task.Image)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return checkImageValue(label, step.Image)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkImageValue rejects an image value beginning with '-', which docker's
// argument parser would read as a flag rather than an image reference.
func checkImageValue(context, image string) error {
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("%s: image %q must not start with '-' (docker would parse it as a flag, not an image reference)", context, image)
	}

	return nil
}

// validateFixAgentImages rejects a fix: agent that sets its own image: —
// agent.RunFix always executes under the failing task's image (rt.Image),
// never the fix agent's own, so a fix agent's image: can never take effect.
// An unresolvable fix: agent name is left for FindAgent to catch at run
// time, same as everywhere else agent/task names aren't cross-checked at
// load time.
func (c *Config) validateFixAgentImages() error {
	check := func(context string, fix *FixSpec) error {
		if fix == nil {
			return nil
		}

		agent, err := c.FindAgent(fix.Agent)
		if err != nil {
			return nil //nolint:nilerr // unresolvable agent name is caught at run time, not here
		}

		if agent.Image != "" {
			return fmt.Errorf("%s: fix agent %q sets image: %q, but a fix loop always runs under the failing task's image, not the fix agent's own", context, fix.Agent, agent.Image)
		}

		return nil
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := check(fmt.Sprintf("task %q", task.Name), task.Fix)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return check(label, step.Fix)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// UsesImages reports whether any resource_type, agent, task, or step sets
// image: — used to fail fast (before any step runs) when docker isn't
// available but the pipeline needs it.
func (c *Config) UsesImages() bool {
	return len(c.Images()) > 0
}

// Images returns every distinct image: this pipeline can execute in, sorted.
//
// Used to pull them all before the first step runs. Without that, the first
// command needing an uncached image pays the pull inside its own step: the
// progress output lands in whatever that command's stderr is being used for
// (a resource check's parsed output, an agent's tool result), and the download
// counts against the step's timeout, so a large image on a cold daemon can
// exhaust a budget meant for the work itself.
func (c *Config) Images() []string {
	seen := map[string]bool{}

	_ = c.visitContainerSettings(func(_ string, settings containerSettings) error {
		if settings.Image != "" {
			seen[settings.Image] = true
		}

		return nil
	})

	images := make([]string, 0, len(seen))
	for image := range seen {
		images = append(images, image)
	}

	slices.Sort(images)

	return images
}
