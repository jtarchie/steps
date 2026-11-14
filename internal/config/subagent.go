package config

// Sub-agent delegation: the agent graph walk that bounds depth and rejects
// cycles, plus where a sub-agent grant may and may not be declared.

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// maxSubAgentDepth bounds how deeply sub-agent tools (see ToolSpec.Agent) may
// nest — a chain of at most this many agents. The bound plus the cycle check
// in walkAgentGraph guarantee sub-agent construction (and merkle recursion)
// terminates. Mirrors secret-agent's agent nesting cap.
const maxSubAgentDepth = 8

// maxVisitsLimit caps how many times a single to:-routed step may execute in
// one run, catching a config mistake (or hostile pipeline) before it burns
// large runtime/cost on a runaway loop — the upper bound paired with the
// existing > 0 lower bound. Loop executions are additive (each backward-
// routing step's runtime visits counter is independent and cumulative for the
// whole segment, never reset by an outer loop re-entering it), so bounding
// each step's max_visits bounds the whole segment's total possible executions.
const maxVisitsLimit = 1000

// validateAgentGraph enforces the sub-agent tool rules at load time: a
// sub-agent tool's shape (no builtin/name/run, never required:, references an
// existing agent), that sub-agents are granted on agents rather than added
// inline on a step, that fix agents grant no sub-agents, and that the agent
// graph has no cycles and does not nest past maxSubAgentDepth.
func (c *Config) validateAgentGraph() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		for _, tool := range agent.Tools {
			if tool.Agent == "" {
				continue
			}

			err := c.validateSubAgentToolShape(fmt.Sprintf("agent %q", agent.Name), tool)
			if err != nil {
				return err
			}
		}
	}

	err := c.rejectInlineSubAgentTools()
	if err != nil {
		return err
	}

	err = c.validateFixAgentSubAgents()
	if err != nil {
		return err
	}

	for i := range c.Agents {
		err := c.walkAgentGraph(c.Agents[i].Name, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateSubAgentToolShape checks one sub-agent tool entry: it must set no
// custom-tool fields, must not be required:, and must reference an agent that
// exists (unlike step-level agent refs, a sub-agent grant is cross-checked at
// load — the graph walk needs the target to exist anyway).
func (c *Config) validateSubAgentToolShape(context string, tool ToolSpec) error {
	if tool.Builtin != "" || tool.Name != "" || tool.Run != "" {
		return fmt.Errorf("%s: sub-agent tool %q must not also set builtin/name/run", context, tool.Agent)
	}

	if tool.Required {
		return fmt.Errorf("%s: sub-agent tool %q may not set required: true", context, tool.Agent)
	}

	_, err := c.FindAgent(tool.Agent)
	if err != nil {
		return fmt.Errorf("%s: sub-agent tool: %w", context, err)
	}

	return nil
}

// rejectInlineSubAgentTools rejects a sub-agent tool added inline on a step
// (or a step hook). A sub-agent is a capability grant, like run_shell: it must
// be declared on the agents: entry and selected by bare name, not introduced
// on the step — otherwise a step could reach an agent the worker never
// granted, and the load-time graph walk (which only sees agents:) couldn't
// bound it.
func (c *Config) rejectInlineSubAgentTools() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			for _, tool := range step.Tools {
				if tool.Agent != "" {
					return fmt.Errorf("%s: sub-agent tool %q must be granted on an agent, not added inline on a step", label, tool.Agent)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateFixAgentSubAgents rejects a fix agent whose grant (or fix: tool
// override) includes a sub-agent tool. A fix loop reproduces and resolves the
// exact failure a task hit; nested delegation inside that loop is out of scope
// for v1, so it's rejected at load rather than silently allowed.
func (c *Config) validateFixAgentSubAgents() error {
	for i := range c.Tasks {
		err := c.checkFixNoSubAgents(fmt.Sprintf("task %q", c.Tasks[i].Name), c.Tasks[i].Fix)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return c.checkFixNoSubAgents(label, step.Fix)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkFixNoSubAgents rejects a fix spec whose own tool override, or whose
// referenced agent's grant, includes a sub-agent tool.
func (c *Config) checkFixNoSubAgents(context string, fix *FixSpec) error {
	if fix == nil {
		return nil
	}

	for _, tool := range fix.Tools {
		if tool.Agent != "" {
			return fmt.Errorf("%s: a fix agent may not use sub-agent tools (tool %q)", context, tool.Agent)
		}
	}

	agent, err := c.FindAgent(fix.Agent)
	if err != nil {
		return nil //nolint:nilerr // unresolvable fix agent is caught at run time, same as validateFixAgentImages
	}

	for _, tool := range agent.Tools {
		if tool.Agent != "" {
			return fmt.Errorf("%s: fix agent %q grants sub-agent tool %q, which a fix agent may not use", context, fix.Agent, tool.Agent)
		}
	}

	return nil
}

// walkAgentGraph does a depth-first walk of the sub-agent graph rooted at
// name, with path holding the ancestor chain (not including name). It reports
// a cycle if name is already an ancestor, and a depth-limit breach once the
// chain would exceed maxSubAgentDepth. An unresolvable name is not a graph
// node — existence of granted sub-agents is checked in
// validateSubAgentToolShape; a bad top-level agents: ref is caught elsewhere.
func (c *Config) walkAgentGraph(name string, path []string) error {
	for _, ancestor := range path {
		if ancestor == name {
			return fmt.Errorf("agent cycle detected: %s -> %s", strings.Join(path, " -> "), name)
		}
	}

	if len(path) >= maxSubAgentDepth {
		return fmt.Errorf("agent nesting depth exceeded (max %d): %s", maxSubAgentDepth, strings.Join(append(append([]string{}, path...), name), " -> "))
	}

	agent, err := c.FindAgent(name)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable ref is not a graph node; existence of grants is checked separately
	}

	path = append(path, name)

	for _, tool := range agent.Tools {
		if tool.Agent == "" {
			continue
		}

		err := c.walkAgentGraph(tool.Agent, path)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateSharedTrees refuses the agent grants whose tools would act on a different copy of the step's tree than the container's (#246): a sub-agent shares its parent's container only when it would build the same one, and a stdio MCP server with a relative cwd: runs on this machine, where an image agent's edits never land.
func (c *Config) validateSharedTrees() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Fix != nil {
				err := c.checkFixMCPCwd(label, *step)
				if err != nil {
					return err
				}
			}

			if step.Agent == "" {
				return nil
			}

			ri, err := c.ResolveAgentInvocation(*step)
			if err != nil {
				return nil //nolint:nilerr // an unresolvable agent is reported by its own check
			}

			return c.checkTreeSharers(label, ri, 0)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkFixMCPCwd: a fix agent works in its task's container, so the relative-cwd refusal follows the task's image.
func (c *Config) checkFixMCPCwd(label string, step Step) error {
	rt, err := c.ResolveTask(step)
	if err != nil || rt.Image == "" || rt.Fix == nil {
		return nil //nolint:nilerr // an unresolvable task is reported by its own check
	}

	specs := rt.Fix.Tools

	agent, err := c.FindAgent(rt.Fix.Agent)
	if err == nil && len(specs) == 0 {
		specs = agent.Tools
	}

	return c.checkMCPCwd(fmt.Sprintf("%s: fix agent %q", label, rt.Fix.Agent), specs)
}

// checkTreeSharers walks ri's sub-agents, refusing one that reaches the tree from a different container; two host agents share this machine's tree whatever their fences. depth stops at a cycle, which validateAgentGraph reports.
func (c *Config) checkTreeSharers(label string, ri ResolvedInvocation, depth int) error {
	if depth > maxSubAgentDepth {
		return nil
	}

	if ri.Image != "" {
		err := c.checkMCPCwd(fmt.Sprintf("%s: agent %q", label, ri.AgentName), ri.ToolSpecs)
		if err != nil {
			return err
		}
	}

	for _, spec := range ri.ToolSpecs {
		if spec.Agent == "" {
			continue
		}

		child, err := c.ResolveAgentInvocation(Step{Agent: spec.Agent})
		if err != nil {
			continue
		}

		if splitsTree(ri, child) {
			return fmt.Errorf("%s: sub-agent %q works on agent %q's files but would run in a different container (image, env, user, network, privileged, container_limits and sandbox must match); see jtarchie/steps#246", label, spec.Agent, ri.AgentName)
		}

		err = c.checkTreeSharers(label, child, depth+1)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkMCPCwd refuses a stdio server with a relative cwd: among specs.
func (c *Config) checkMCPCwd(context string, specs []ToolSpec) error {
	for _, spec := range specs {
		srv, err := c.FindMCPServer(spec.MCP)
		if spec.MCP == "" || err != nil {
			continue
		}

		if srv.IsStdio() && srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
			return fmt.Errorf("%s runs in a container, but mcp server %q (cwd: %s) runs on this machine against a copy of the tree its edits never reach; use an absolute cwd: or an HTTP server (jtarchie/steps#246)", context, spec.MCP, srv.Cwd)
		}
	}

	return nil
}

// splitsTree reports a child whose tools would act on another copy of the parent's tree: one of the two is containerized, the child reaches the tree, and their containers differ.
func splitsTree(parent, child ResolvedInvocation) bool {
	return (parent.Image != "" || child.Image != "") && ReachesTree(child.ToolSpecs) && !sameContainer(parent, child)
}

// sameContainer: the settings a child's runner is built from, so equal ones can share the parent's.
func sameContainer(a, b ResolvedInvocation) bool {
	return a.Image == b.Image && slices.Equal(a.Env, b.Env) && reflect.DeepEqual(a.Isolation(), b.Isolation())
}
