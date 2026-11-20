package config

// unfenced: is the pipeline's half of fence: enforce's opt-out (jtarchie/steps#263). It is written where sandbox: or image: would be, on the entity that would carry the fence, and names why that entity runs unfenced. On its own it loosens nothing: a daemon under enforce accepts the entity only when its own config's allow-unfenced: names the pipeline and the entity too, so a pipeline cannot certify itself. HostCommand.Entity is the spelling both halves meet on.

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The entity kinds an allow-unfenced: entry names, each followed by a space and the entity's name.
const (
	EntityTask         = "task"
	EntityAgent        = "agent"
	EntityResourceType = "resource_type"
	EntityMCPServer    = "mcp_server"
	EntityMCPExport    = "mcp_export"
	// EntityGuard is a when: guard with no entity of its own (a block, get, load_var:, approval:), named by its job: no step has a name that survives an edit above it.
	EntityGuard = "guard"
)

// EntityKinds is every word an entity may start with.
func EntityKinds() []string {
	return []string{EntityTask, EntityAgent, EntityResourceType, EntityMCPServer, EntityMCPExport, EntityGuard}
}

func entity(kind, name string) string { return kind + " " + name }

// reasonOf is the first declared reason, empty when none is.
func reasonOf(declared ...*string) string {
	for _, reason := range declared {
		if reason != nil {
			return *reason
		}
	}

	return ""
}

// taskReason is the step's own unfenced:, else its tasks: entry's; an inline run: has only its own.
func (c *Config) taskReason(step Step) string {
	declared := []*string{step.Unfenced}

	if task := c.taskEntry(step); task != nil {
		declared = append(declared, task.Unfenced)
	}

	return reasonOf(declared...)
}

// agentReason is the step's own unfenced:, else its agents: entry's.
func (c *Config) agentReason(step Step, name string) string {
	declared := []*string{step.Unfenced}

	agent, err := c.FindAgent(name)
	if err == nil {
		declared = append(declared, agent.Unfenced)
	}

	return reasonOf(declared...)
}

// validateUnfenced holds every unfenced: to a reason somebody can read, and keeps it off a step where it covers nothing.
func (c *Config) validateUnfenced() error {
	declared := map[string]*string{}

	for _, task := range c.Tasks {
		declared[fmt.Sprintf("task %q", task.Name)] = task.Unfenced
	}

	for _, agent := range c.Agents {
		declared[fmt.Sprintf("agent %q", agent.Name)] = agent.Unfenced
	}

	for _, rt := range c.ResourceTypes {
		declared[fmt.Sprintf("resource_type %q", rt.Name)] = rt.Unfenced
	}

	for _, server := range c.MCPServers {
		declared[fmt.Sprintf("mcp_servers %q", server.Name)] = server.Unfenced
	}

	for _, export := range c.MCPExports {
		declared[fmt.Sprintf("mcp_exports %q", export.Name)] = export.Unfenced
	}

	for _, where := range slices.Sorted(maps.Keys(declared)) {
		err := checkReason(where, declared[where])
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(checkStepUnfenced)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkStepUnfenced(label string, step *Step) error {
	if step.Unfenced == nil {
		return nil
	}

	err := checkReason(label, step.Unfenced)
	if err != nil {
		return err
	}

	switch kind, _ := step.Kind(); kind {
	case StepKindTask, StepKindAgent:
		return nil
	case StepKindPut:
		return fmt.Errorf("%s: unfenced: on a put covers nothing; its commands, guard included, run under its resource type, so set it on the resource_types: entry", label)
	case StepKindGet, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindDo, StepKindLoadVar, StepKindApproval, StepKindAsk:
	}

	if step.Guarded() {
		return nil
	}

	return fmt.Errorf("%s: unfenced: covers a step's commands, and this step runs none without a when: guard; drop it", label)
}

func checkReason(where string, reason *string) error {
	if reason != nil && strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("%s: unfenced: needs a reason — it is what the operator and every reader of the pipeline see beside the command", where)
	}

	return nil
}
