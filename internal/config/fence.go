package config

// What a daemon's fence: policy judges (jtarchie/steps#263): every place a
// pipeline runs a command that nothing fences. It is HostCommands with the
// two fences --require-image does not count, and the three holes it does not
// look for:
//
//   - sandbox: true is a fence, but only where the OS sandbox exists, which is
//     the caller's machine fact to supply.
//   - image: with privileged: true is a container with the host's privileges.
//   - an mcp_exports: entry's custom commands, and a CLI agent with
//     settings: project, whose repo hooks run on this machine whatever image:
//     its tools run in.
//
// Placement needs no rule of its own: a docker+ worker (docker+ssh://, aws://,
// gcp://, do://, local:) runs only steps with an image:, which is already the
// fence, and a bare ssh:// worker runs only steps without one.

import (
	"fmt"
	"slices"
)

// Unfenced lists every command this pipeline would run unfenced on a machine where the OS sandbox exists (sandboxHere) or does not.
func (c *Config) Unfenced(sandboxHere bool) []HostCommand {
	var found []HostCommand

	for _, host := range c.HostCommands() {
		if host.Sandboxed {
			if sandboxHere {
				continue
			}

			host.Why = "sandbox: true is a fence only where the OS sandbox exists (darwin), and this machine has none; set image:"
		}

		found = append(found, host)
	}

	found = append(found, c.privilegedContainers()...)
	found = append(found, c.hostExports()...)

	for _, agent := range c.projectSettingsAgents() {
		if !slices.ContainsFunc(found, func(h HostCommand) bool { return h.Where == agent.Where }) {
			found = append(found, agent)
		}
	}

	return found
}

const privilegedWhy = "image: with privileged: true gives the container the host's privileges; drop privileged:"

// privilegedContainers is every container run with the host's privileges: a resource whose type asks, and a task or agent step that resolves to it.
func (c *Config) privilegedContainers() []HostCommand {
	var found []HostCommand

	for _, resource := range c.Resources {
		rt, err := c.FindResourceType(resource.Type)
		if err == nil && rt.Image != "" && rt.Privileged {
			found = append(found, HostCommand{Name: resource.Name, Where: fmt.Sprintf("resource %q", resource.Name), Why: fmt.Sprintf("type %q: %s", resource.Type, privilegedWhy)})
		}
	}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(label string, step *Step) error {
			found = append(found, c.privilegedStep(label, step)...)

			return nil
		})
	}

	return found
}

func (c *Config) privilegedStep(label string, step *Step) []HostCommand {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		task, err := c.ResolveTask(*step)
		if err == nil && task.Image != "" && task.Privileged {
			return []HostCommand{{Name: step.Task, Where: fmt.Sprintf("task %q in %s", step.Task, label), Why: privilegedWhy}}
		}
	case StepKindAgent:
		invocation, err := c.ResolveAgentInvocation(*step)
		if err == nil && invocation.Image != "" && invocation.Privileged {
			return []HostCommand{{Name: step.Agent, Where: fmt.Sprintf("agent %q in %s", step.Agent, label), Why: privilegedWhy}}
		}
	case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindDo:
	}

	return nil
}

// hostExports is every mcp_exports: entry whose custom run: tools run on the daemon's machine, or in a privileged container.
func (c *Config) hostExports() []HostCommand {
	var found []HostCommand

	for _, export := range c.MCPExports {
		if !export.hasCommands() {
			continue
		}

		where := fmt.Sprintf("mcp_exports %q", export.Name)

		switch {
		case export.Image == "":
			found = append(found, HostCommand{Name: export.Name, Where: where, Why: "its custom run: tools run on the host; set image:"})
		case export.Privileged:
			found = append(found, HostCommand{Name: export.Name, Where: where, Why: privilegedWhy})
		}
	}

	return found
}

// projectSettingsAgents is every CLI agent step that loads the repo's .claude/ hooks, which run on this machine whatever fences its tools.
func (c *Config) projectSettingsAgents() []HostCommand {
	var found []HostCommand

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(label string, step *Step) error {
			if step.Agent == "" {
				return nil
			}

			invocation, err := c.ResolveAgentInvocation(*step)
			if err == nil && invocation.CLISettings == CLISettingsProject {
				found = append(found, HostCommand{
					Name: step.Agent, Where: fmt.Sprintf("agent %q in %s", step.Agent, label),
					Why: "settings: project runs the repo's .claude/ hooks on this machine, outside image: and sandbox:",
				})
			}

			return nil
		})
	}

	return found
}
