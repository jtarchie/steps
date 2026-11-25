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
	"strings"
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

			host.Why = "sandbox: true is a fence only where the OS sandbox exists (darwin, or linux with Landlock ABI 3+), and this machine has none; set image:"
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
				found = append(found, c.hostGuard(label, job.Name, *step)...)
			}

			return nil
		})
	}

	return found
}

func (c *Config) hostGuard(label, job string, guarded Step) []HostCommand {
	where := "when: guard of " + label
	step := guarded.Unwrap()

	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		// A host task is listed itself, and its guard shares its fence.
		return nil
	case StepKindAgent:
		invocation, err := c.ResolveAgentInvocation(step)
		if err != nil || invocation.Image != "" || len(c.hostStep(label, &step)) > 0 {
			return nil
		}

		return []HostCommand{{
			Name: step.Agent, Where: where, Why: guardWhy + "; set image: on the agent" + sandboxed(invocation.Sandbox), Sandboxed: invocation.Sandbox,
			Entity: entity(EntityAgent, step.Agent), Reason: c.agentReason(step, step.Agent),
		}}
	case StepKindPut:
		return c.hostPutGuard(where, step)
	case StepKindGet, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindAsk, StepKindDo:
	}

	guard := HostCommand{
		Where: where, Why: guardWhy + ", and this step has no image: or sandbox: to run it under; move the check into a task",
		Entity: entity(EntityGuard, job), Reason: reasonOf(guarded.Unfenced),
	}

	if step.Sandboxed() {
		guard.Why, guard.Sandboxed = guardWhy+sandboxed(true), true
	}

	return []HostCommand{guard}
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

	return []HostCommand{{
		Name: resource.Name, Where: where, Why: fmt.Sprintf("%s, under type %q; give the type an image:", guardWhy, resource.Type) + sandboxed(fenced), Sandboxed: fenced,
		Entity: entity(EntityResourceType, resource.Type), Reason: reasonOf(rt.Unfenced),
	}}
}

const privilegedWhy = "image: with privileged: true gives the container the host's privileges; drop privileged:"

// hostReach is why a container is no fence, or "": privileged: true holds the host's privileges, and network: host or container:<name> its network — the daemon's own API on loopback, a cloud's metadata address — which is what an image: is meant to keep a step from.
func hostReach(privileged bool, network string) string {
	switch {
	case privileged:
		return privilegedWhy
	case network == "host" || strings.HasPrefix(network, "container:"):
		return "image: with network: " + network + " shares a network the container's own would keep it out of (the daemon's loopback, a cloud's metadata address); drop network: or give it a network of its own"
	}

	return ""
}

// privilegedContainers is every container run with the host's privileges or on its network (hostReach): a resource whose type asks, and a task or agent step that resolves to it.
func (c *Config) privilegedContainers() []HostCommand {
	var found []HostCommand

	for _, resource := range c.Resources {
		rt, err := c.FindResourceType(resource.Type)
		if err != nil || rt.Image == "" {
			continue
		}

		if why := hostReach(rt.Privileged, rt.Network); why != "" {
			found = append(found, HostCommand{
				Name: resource.Name, Where: fmt.Sprintf("resource %q", resource.Name), Why: fmt.Sprintf("type %q: %s", resource.Type, why),
				Entity: entity(EntityResourceType, resource.Type), Reason: reasonOf(rt.Unfenced),
			})
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
		if why := hostReach(task.Privileged, task.Network); err == nil && task.Image != "" && why != "" {
			return []HostCommand{{Name: step.Task, Where: fmt.Sprintf("task %q in %s", step.Task, label), Why: why, Entity: entity(EntityTask, step.Task), Reason: c.taskReason(*step)}}
		}
	case StepKindAgent:
		invocation, err := c.ResolveAgentInvocation(*step)
		if why := hostReach(invocation.Privileged, invocation.Network); err == nil && invocation.Image != "" && why != "" {
			return []HostCommand{{Name: step.Agent, Where: fmt.Sprintf("agent %q in %s", step.Agent, label), Why: why, Entity: entity(EntityAgent, step.Agent), Reason: c.agentReason(*step, step.Agent)}}
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

		self := HostCommand{Name: export.Name, Where: where, Entity: entity(EntityMCPExport, export.Name), Reason: reasonOf(export.Unfenced)}

		if export.hasCommands() {
			switch {
			case export.Image == "":
				self.Why = "its custom run: tools run on the host; set image:"
				found = append(found, self)
			case hostReach(export.Privileged, export.Network) != "":
				self.Why = hostReach(export.Privileged, export.Network)
				found = append(found, self)
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
	reason := c.agentReason(Step{}, spec.Agent)

	if why := hostReach(invocation.Privileged, invocation.Network); invocation.Image != "" && why != "" {
		return []HostCommand{{Name: spec.Agent, Where: where, Why: why, Entity: entity(EntityAgent, spec.Agent), Reason: reason}}
	}

	return c.hostAgent(where, invocation, reason)
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
					Why:    "settings: project runs the repo's .claude/ hooks on this machine, outside image: and sandbox:",
					Entity: entity(EntityAgent, step.Agent), Reason: c.agentReason(*step, step.Agent),
				})
			}

			return nil
		})
	}

	return found
}
