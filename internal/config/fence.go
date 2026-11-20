package config

// What a daemon's fence: policy judges (jtarchie/steps#263): every place a
// pipeline runs a command that nothing fences. It is HostCommands with the
// two fences --require-image does not count, and the three holes it does not
// look for:
//
//   - sandbox: true is a fence, but only where the OS sandbox exists, which is
//     the caller's machine fact to supply.
//   - image: with privileged: true is a container with the host's privileges.
//   - an mcp_exports: entry's custom commands and exported agents, and a CLI agent with
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

	hosts := c.HostCommands()
	hosts = append(hosts, c.hostExports()...)
	hosts = append(hosts, c.hostGuards()...)

	for _, host := range hosts {
		if host.Sandboxed {
			if sandboxHere {
				continue
			}

			host.Why = "sandbox: true is a fence only where the OS sandbox exists (darwin), and this machine has none; set image:"
		}

		found = append(found, host)
	}

	found = append(found, c.privilegedContainers()...)

	for _, agent := range c.projectSettingsAgents() {
		if !slices.ContainsFunc(found, func(h HostCommand) bool { return h.Where == agent.Where }) {
			found = append(found, agent)
		}
	}

	return found
}

const guardWhy = "its when: command runs on the host"

// hostGuards is every when: guard that runs on the host where nothing else names the place: one runs where its step would (pipeline.resolveStepRuntime), so a host task is already listed, and a step with no command of its own runs its guard on the host with no fence at all.
func (c *Config) hostGuards() []HostCommand {
	var found []HostCommand

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(label string, step *Step) error {
			if step.Guarded() {
				found = append(found, c.hostGuard(label, step.Unwrap())...)
			}

			return nil
		})
	}

	return found
}

func (c *Config) hostGuard(label string, step Step) []HostCommand {
	where := "when: guard of " + label

	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		// A host task is listed itself, and its guard shares its fence.
		return nil
	case StepKindAgent:
		invocation, err := c.ResolveAgentInvocation(step)
		if err != nil || invocation.Image != "" || len(c.hostStep(label, &step)) > 0 {
			return nil
		}

		return []HostCommand{{Name: step.Agent, Where: where, Why: guardWhy + "; set image: on the agent" + sandboxed(invocation.Sandbox), Sandboxed: invocation.Sandbox}}
	case StepKindPut:
		return c.hostPutGuard(where, step)
	case StepKindGet, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindAsk, StepKindDo:
	}

	if step.Sandboxed() {
		return []HostCommand{{Where: where, Why: guardWhy + sandboxed(true), Sandboxed: true}}
	}

	return []HostCommand{{Where: where, Why: guardWhy + ", and this step has no image: or sandbox: to run it under; move the check into a task"}}
}

// hostPutGuard: a put's guard runs under its resource type, as its out would.
func (c *Config) hostPutGuard(where string, step Step) []HostCommand {
	resource, err := c.FindResource(step.PutResourceName())
	if err != nil {
		return nil
	}

	rt, err := c.FindResourceType(resource.Type)
	if err != nil || rt.Image != "" {
		return nil
	}

	fenced := rt.Sandbox || step.Sandboxed()

	return []HostCommand{{Name: resource.Name, Where: where, Why: fmt.Sprintf("%s, under type %q; give the type an image:", guardWhy, resource.Type) + sandboxed(fenced), Sandboxed: fenced}}
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
	case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindAsk, StepKindDo:
	}

	return nil
}

// hostExports is every mcp_exports: entry whose custom run: tools, or whose exported agents' tools, run on the daemon's machine or in a privileged container.
func (c *Config) hostExports() []HostCommand {
	var found []HostCommand

	for _, export := range c.MCPExports {
		where := fmt.Sprintf("mcp_exports %q", export.Name)

		if export.hasCommands() {
			switch {
			case export.Image == "":
				found = append(found, HostCommand{Name: export.Name, Where: where, Why: "its custom run: tools run on the host; set image:"})
			case export.Privileged:
				found = append(found, HostCommand{Name: export.Name, Where: where, Why: privilegedWhy})
			}
		}

		for _, spec := range export.Tools {
			found = append(found, c.exportedAgent(where, spec)...)
		}
	}

	return found
}

// exportedAgent is an exported agent tool that runs commands unfenced: a host agent builds its runner from its own entry, as a sub-agent does.
func (c *Config) exportedAgent(export string, spec ToolSpec) []HostCommand {
	if spec.Agent == "" {
		return nil
	}

	// As agent.newCallSpace resolves it: no step, so the entry alone.
	invocation, err := c.ResolveAgentInvocation(Step{Agent: spec.Agent})
	if err != nil {
		return nil
	}

	where := fmt.Sprintf("%s agent %q", export, spec.Agent)

	if invocation.Image != "" && invocation.Privileged {
		return []HostCommand{{Name: spec.Agent, Where: where, Why: privilegedWhy}}
	}

	return c.hostAgent(where, invocation)
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
