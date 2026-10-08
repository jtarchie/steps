package config

// memory: on an agent step — the scope it keeps facts under between runs, and
// the two builtins that write them.

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// StepMemory is an agent step's memory: which scope its entries are filed
// under. The scope comes from a FILE the pipeline names, never from the
// prompt or the model: a message saying "tell me what you know about Dana"
// has no way to reach Dana's entries, because nothing it says picks the
// scope.
type StepMemory struct {
	// ScopeFrom is <input>/<path>: a file in one of the step's inputs whose
	// content (trimmed) is the scope — a Slack user id, say. Read when the
	// step runs; an empty or missing file fails the step rather than falling
	// back to a scope everyone shares.
	ScopeFrom string `yaml:"scope_from"`
}

// RememberBuiltinName and ForgetBuiltinName are the builtins that write a
// step's memory. Named as constants for the reason AskUserBuiltinName is:
// config validates where they may be granted and internal/agent implements
// them, and the two must agree on the spelling.
//
// Both are absent from DefaultAgentToolSpecs. What a bot keeps about a
// person is decided by whoever holds these, and that is a choice the
// pipeline makes in writing.
const (
	RememberBuiltinName = "remember"
	ForgetBuiltinName   = "forget"
)

// GrantsMemory reports whether a resolved tool grant writes memory.
func GrantsMemory(specs []ToolSpec) bool {
	return slices.ContainsFunc(specs, func(spec ToolSpec) bool {
		return spec.Builtin == RememberBuiltinName || spec.Builtin == ForgetBuiltinName
	})
}

// DefaultMemoryEntries is how many entries one scope keeps when the pipeline
// does not say, the oldest going first.
const DefaultMemoryEntries = 100

// MemoryEntriesLimit is how many entries each scope keeps. Zero from the
// pipeline means no limit, as for every other cap here.
func (c *Config) MemoryEntriesLimit() int {
	if c.Defaults != nil && c.Defaults.MemoryEntries != nil {
		return *c.Defaults.MemoryEntries
	}

	return DefaultMemoryEntries
}

// ScopeInput splits scope_from into the input it names and the path inside
// it.
func (m StepMemory) ScopeInput() (string, string) {
	input, rel, _ := strings.Cut(m.ScopeFrom, "/")

	return input, rel
}

// validateMemory holds memory: and the builtins that write it to the one
// place they mean something: a plan step's agent, scoped by a file it was
// handed.
//
// A hook is refused because it has no inputs: of its own to read a scope
// from. A sub-agent, a fix: agent and an ask_user responder are refused a
// memory grant because none of them is a step that declares memory:, so
// their remember would have no scope to file under.
func (c *Config) validateMemory() error {
	if c.Defaults != nil && c.Defaults.MemoryEntries != nil && *c.Defaults.MemoryEntries < 0 {
		return fmt.Errorf("defaults: memory_entries must not be negative (omit it for the default of %d, or set 0 for no limit)", DefaultMemoryEntries)
	}

	for i := range c.Jobs {
		err := c.Jobs[i].visitHookSteps(c.rejectMemoryOnHook)
		if err != nil {
			return err
		}

		err = c.Jobs[i].visitSteps(c.checkMemoryStep)
		if err != nil {
			return err
		}
	}

	return c.rejectDelegatedMemoryGrants()
}

func (c *Config) rejectMemoryOnHook(label string, step *Step) error {
	if step.Memory != nil {
		return fmt.Errorf("%s: memory is not valid on hook steps — a hook has no inputs: to read its scope from", label)
	}

	if step.Agent != "" && c.stepGrantsMemory(*step) {
		return fmt.Errorf("%s (agent %q): %s and %s are not valid on a hook step — they write a memory: scope, which only a plan step declares",
			label, step.Agent, RememberBuiltinName, ForgetBuiltinName)
	}

	return nil
}

func (c *Config) checkMemoryStep(label string, step *Step) error {
	if step.Memory == nil {
		if step.Agent != "" && c.stepGrantsMemory(*step) {
			return fmt.Errorf("%s (agent %q): %s and %s need memory: on the step, naming the file whose content is the scope they write to (memory: {scope_from: <input>/<file>})",
				label, step.Agent, RememberBuiltinName, ForgetBuiltinName)
		}

		return nil
	}

	if step.Agent == "" {
		return fmt.Errorf("%s: memory is only valid on agent steps (nothing else holds a conversation to remember from)", label)
	}

	return checkScopeFrom(label, *step)
}

func checkScopeFrom(label string, step Step) error {
	input, rel := step.Memory.ScopeInput()

	if input == "" || rel == "" {
		return fmt.Errorf("%s: memory.scope_from %q must be <input>/<file>, naming a file in one of this step's inputs", label, step.Memory.ScopeFrom)
	}

	if path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s: memory.scope_from %q must name a file inside input %q, written without . or .. segments", label, step.Memory.ScopeFrom, input)
	}

	if !slices.Contains(step.InputNames(), input) {
		return fmt.Errorf("%s: memory.scope_from names input %q, which is not in this step's inputs: — add it there", label, input)
	}

	return nil
}

// stepGrantsMemory is whether the tools this step will actually run with
// write memory. A resolution failure answers false: the validator that owns
// it reports it with the context this one lacks.
func (c *Config) stepGrantsMemory(step Step) bool {
	ri, err := c.ResolveAgentInvocation(step)
	if err != nil {
		return false
	}

	return GrantsMemory(ri.ToolSpecs)
}

// rejectDelegatedMemoryGrants refuses a memory grant on an agent invoked as a
// sub-agent, a fix: agent or an ask_user responder.
func (c *Config) rejectDelegatedMemoryGrants() error {
	for _, role := range c.delegatedAgents() {
		agent, err := c.FindAgent(role.agent)
		if err != nil {
			continue
		}

		if GrantsMemory(agent.Tools) {
			return fmt.Errorf("agent %q: %s and %s are not valid on an agent used as %s — only an agent step that declares memory: has a scope for them to write to",
				role.agent, RememberBuiltinName, ForgetBuiltinName, role.as)
		}
	}

	return nil
}

type delegatedAgent struct {
	agent string
	as    string
}

func (c *Config) delegatedAgents() []delegatedAgent {
	var roles []delegatedAgent

	fromSpecs := func(specs []ToolSpec) {
		for _, spec := range specs {
			if spec.Agent != "" {
				roles = append(roles, delegatedAgent{spec.Agent, "a sub-agent"})
			}

			if spec.AnsweredBy != "" {
				roles = append(roles, delegatedAgent{spec.AnsweredBy, "an ask_user responder"})
			}
		}
	}

	for _, agent := range c.Agents {
		fromSpecs(agent.Tools)
	}

	for _, task := range c.Tasks {
		if task.Fix != nil {
			roles = append(roles, delegatedAgent{task.Fix.Agent, "a fix: agent"})
		}
	}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			fromSpecs(step.Tools)

			if step.Fix != nil {
				roles = append(roles, delegatedAgent{step.Fix.Agent, "a fix: agent"})
			}

			return nil
		})
	}

	return roles
}
