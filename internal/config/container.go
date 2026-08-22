package config

// The settings that describe HOW a command executes rather than what it does —
// image:, env:, user:, network: — and the one walk their load-time checks share.

import "fmt"

// containerSettings is the execution-shape group as it appears on any entity
// that can carry it: a resource_type, a task, an agent, or a step overriding
// one of those. Collecting them into a value lets every rule about them be
// written once against a single walk, rather than each rule repeating the
// four loops over ResourceTypes/Agents/Tasks/steps.
type containerSettings struct {
	Image      string
	Env        []string
	User       string
	Network    string
	Privileged bool
	Limits     *ContainerLimits
	// Tags rides along so the rule that refuses tags: with image: reads the
	// RESOLVED image, which is the only place a step inheriting one from the
	// tasks: entry it references can be caught.
	Tags []string
}

func (rt ResourceType) containerSettings() containerSettings {
	return containerSettings{Image: rt.Image, Env: rt.Env, User: rt.User, Network: rt.Network, Privileged: rt.Privileged, Limits: rt.Limits}
}

func (a Agent) containerSettings() containerSettings {
	return containerSettings{Image: a.Image, Env: a.Env, User: a.User, Network: a.Network, Privileged: a.Privileged, Limits: a.Limits}
}

func (t Task) containerSettings() containerSettings {
	return containerSettings{Image: t.Image, Env: t.Env, User: t.User, Network: t.Network, Privileged: t.Privileged, Limits: t.Limits}
}

func (s Step) containerSettings() containerSettings {
	return containerSettings{Image: s.Image, Env: s.Env, User: s.User, Network: s.Network, Privileged: s.Privileged, Limits: s.Limits, Tags: s.Tags}
}

// visitContainerSettings calls fn for every entity that can carry execution
// settings, with a context string naming it for error messages. Steps come
// last and are reached through visitSteps, so nested steps (hooks, branches of
// a concurrent block, a try: body) are covered exactly as they are everywhere
// else.
func (c *Config) visitContainerSettings(fn func(context string, settings containerSettings) error) error {
	for i := range c.ResourceTypes {
		rt := c.ResourceTypes[i]

		err := fn(fmt.Sprintf("resource_type %q", rt.Name), rt.containerSettings())
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		agent := c.Agents[i]

		err := fn(fmt.Sprintf("agent %q", agent.Name), agent.containerSettings())
		if err != nil {
			return err
		}
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := fn(fmt.Sprintf("task %q", task.Name), task.containerSettings())
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			settings := step.containerSettings()
			// A step's own image: is usually empty even when it runs
			// containerized, because the image comes from the tasks:/agents:
			// entry it references. Resolving it here means every rule reads
			// the image the step will actually run under, rather than each
			// rule that cares having to remember to resolve it.
			settings.Image = c.resolvedStepImage(*step)

			return fn(label, settings)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// rejectOnGetAndPut is the shared placement rule for every execution setting:
// they describe how a task's or agent's own command runs, so a get (which has
// no such command) and a put (whose command comes from its resource type)
// can't scope one. A try: wrapper is refused for a different reason — the
// value would be accepted and then ignored, since resolution reads the wrapped
// step, never the wrapper.
//
// set reports whether this step declared the setting at all, which differs per
// field: user:/image: use non-empty, env: uses non-nil (an explicit `env: []`
// is a real declaration).
func (c *Config) rejectOnGetAndPut(field string, set func(*Step) bool) error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if !set(step) {
				return nil
			}

			//kindswitch:ignore Task and Agent are the kinds these settings are FOR — the cases here are the rejections
			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): %s is not valid on get steps; set it on the resource_type instead", label, step.Get, field)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): %s is not valid on put steps; set it on the resource_type instead", label, step.Put, field)
			case step.Try != nil:
				return fmt.Errorf("%s: %s is not valid on a try: step; set it on the step try: wraps", label, field)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// resolvedStepImage reports the image a task/agent step would actually run
// under, merging the step's own image: over the entry it references. It
// answers "" for anything it cannot resolve, leaving that error to whichever
// validator owns it — an unknown task/agent name is not this walk's to report.
func (c *Config) resolvedStepImage(step Step) string {
	if step.Image != "" {
		return step.Image
	}

	//kindswitch:ignore only task/agent reference an entry carrying an image; a put's comes from its resource type and every rule that consults this rejects put/get first
	switch {
	case step.Task != "":
		task, err := c.FindTask(step.Task)
		if err != nil {
			return ""
		}

		return task.Image
	case step.Agent != "":
		agent, err := c.FindAgent(step.Agent)
		if err != nil {
			return ""
		}

		return agent.Image
	default:
		return ""
	}
}
