package config

import (
	"embed"
	"fmt"
	"log/slog"
	"strings"
)

//go:embed prompts/*.md agents/*.yml tools/*.md resource_types/*.yml
var builtinFS embed.FS

// ReadBuiltinPrompt returns the content of a named built-in system prompt.
// name is like "builder" — resolves to "prompts/<name>.md".
func ReadBuiltinPrompt(name string) (string, error) {
	data, err := builtinFS.ReadFile("prompts/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("built-in prompt %q not found", name)
	}
	return string(data), nil
}

// ReadBuiltinAgent returns the parsed YAML of a named built-in agent profile.
// name is like "builder" — resolves to "agents/<name>.yml".
func ReadBuiltinAgent(name string) (Agent, error) {
	data, err := builtinFS.ReadFile("agents/" + name + ".yml")
	if err != nil {
		return Agent{}, fmt.Errorf("built-in agent %q not found", name)
	}
	var agent Agent

	err = strictUnmarshal(data, &agent)
	if err != nil {
		return Agent{}, fmt.Errorf("built-in agent %q: %w", name, err)
	}
	return agent, nil
}

// ReadBuiltinToolDescription returns the description for a named built-in
// tool. name is like "read_file" — resolves to "tools/<name>.md".
func ReadBuiltinToolDescription(name string) (string, error) {
	data, err := builtinFS.ReadFile("tools/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("built-in tool description %q not found", name)
	}
	return string(data), nil
}

// ListBuiltinAgentNames returns the available built-in agent profile names.
func ListBuiltinAgentNames() ([]string, error) {
	entries, err := builtinFS.ReadDir("agents")
	if err != nil {
		return nil, fmt.Errorf("reading built-in agent dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			name := e.Name()
			if len(name) > 4 && name[len(name)-4:] == ".yml" {
				names = append(names, name[:len(name)-4])
			}
		}
	}
	return names, nil
}

// registerBuiltinAgents makes every built-in agent profile available as
// @builtin/<name>, and merges a user's same-named entry ON TOP of the profile
// rather than replacing it.
//
// The merge is what makes these usable at all. Built-in profiles ship a
// persona and a tool grant but no source: — they can't, since the model to
// call is the one thing only the user knows. Under the previous
// all-or-nothing rule, naming @builtin/reviewer in agents: to supply that
// model discarded the whole profile, so the only way to get a model was to
// lose the prompt the profile existed for. Now the entry supplies what it
// sets and inherits the rest:
//
//	agents:
//	- name: "@builtin/reviewer"
//	  source: { model: lmstudio/qwen }   # persona and tools come from the profile
//
// It reuses mergeAgentDocument, so an @builtin reference behaves exactly like
// a file: include that happens to live in the embedded FS — same "inline wins"
// semantics, no second merge philosophy to learn.
func (c *Config) registerBuiltinAgents() {
	builtinNames, err := ListBuiltinAgentNames()
	if err != nil {
		slog.Warn("builtin.agents.list", "error", err)

		return
	}

	for _, name := range builtinNames {
		fullName := "@builtin/" + name

		agent, err := ReadBuiltinAgent(name)
		if err != nil {
			slog.Warn("builtin.agents.load", "name", name, "error", err)

			continue
		}

		err = resolveBuiltinAgentSystem(&agent)
		if err != nil {
			slog.Warn("builtin.agents.resolve", "name", name, "error", err)

			continue
		}

		agent.Name = fullName

		existing := c.findAgentIndex(fullName)
		if existing >= 0 {
			mergeAgentDocument(&c.Agents[existing], agent)
			slog.Debug("builtin.agents.merge", "name", fullName)

			continue
		}

		c.Agents = append(c.Agents, agent)
		slog.Debug("builtin.agents.register", "name", fullName)
	}
}

// ListBuiltinResourceTypeNames returns the available built-in resource type
// names.
func ListBuiltinResourceTypeNames() ([]string, error) {
	entries, err := builtinFS.ReadDir("resource_types")
	if err != nil {
		return nil, fmt.Errorf("reading built-in resource type dir: %w", err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".yml")
		if !entry.IsDir() && ok {
			names = append(names, name)
		}
	}

	return names, nil
}

// ReadBuiltinResourceType returns the parsed YAML of a named built-in resource
// type. name is like "git" — resolves to "resource_types/<name>.yml".
func ReadBuiltinResourceType(name string) (ResourceType, error) {
	data, err := builtinFS.ReadFile("resource_types/" + name + ".yml")
	if err != nil {
		return ResourceType{}, fmt.Errorf("built-in resource type %q not found", name)
	}

	var resourceType ResourceType

	err = strictUnmarshal(data, &resourceType)
	if err != nil {
		return ResourceType{}, fmt.Errorf("built-in resource type %q: %w", name, err)
	}

	return resourceType, nil
}

// registerBuiltinResourceTypes makes every built-in resource type available
// under its bare name, so `type: git` needs no resource_types: block at all.
//
// It exists because there were no built-in types: cloning a repository — step
// two of essentially every real pipeline — meant hand-writing check/in shell
// against an undocumented JSON contract. The three examples in this repo each
// wrote their own, and none of them agreed.
//
// Unlike a built-in agent (which merges — see registerBuiltinAgents), a
// user-defined type of the same name replaces this one outright. A type is a
// set of command templates, and half of one command paired with half of
// another is not a resource type anyone meant to write.
func (c *Config) registerBuiltinResourceTypes() {
	builtinNames, err := ListBuiltinResourceTypeNames()
	if err != nil {
		slog.Warn("builtin.resource_types.list", "error", err)

		return
	}

	for _, name := range builtinNames {
		if c.findResourceTypeIndex(name) >= 0 {
			slog.Debug("builtin.resource_types.shadowed", "name", name)

			continue
		}

		resourceType, err := ReadBuiltinResourceType(name)
		if err != nil {
			slog.Warn("builtin.resource_types.load", "name", name, "error", err)

			continue
		}

		resourceType.Name = name
		c.ResourceTypes = append(c.ResourceTypes, resourceType)
		slog.Debug("builtin.resource_types.register", "name", name)
	}
}

// findResourceTypeIndex returns the index of the resource type with the given
// name, or -1.
func (c *Config) findResourceTypeIndex(name string) int {
	for i := range c.ResourceTypes {
		if c.ResourceTypes[i].Name == name {
			return i
		}
	}

	return -1
}

// resolveBuiltinAgentSystem resolves a built-in agent's system_file reference
// into its system field. Built-in agents reference @builtin/ prompts that
// exist only in the embedded FS, so they must be resolved at registration time
// rather than through the usual file-include mechanism.
func resolveBuiltinAgentSystem(a *Agent) error {
	if a.SystemFile == "" {
		return nil
	}

	if !strings.HasPrefix(a.SystemFile, "@builtin/") {
		return fmt.Errorf("built-in agent system_file must reference @builtin/, got %q", a.SystemFile)
	}

	prompt, err := ReadBuiltinPrompt(strings.TrimPrefix(a.SystemFile, "@builtin/"))
	if err != nil {
		return err
	}

	a.System = prompt
	a.SystemFile = ""
	return nil
}

// findAgentIndex returns the index of the agent with the given name, or -1.
func (c *Config) findAgentIndex(name string) int {
	for i := range c.Agents {
		if c.Agents[i].Name == name {
			return i
		}
	}

	return -1
}

// resolveSubAgentDescriptions walks every agent's tool spec and, for any
// sub-agent tool (spec.Agent != "") that has no inline Description, fills it
// from the referenced child agent's own Description. If the child agent also
// lacks a Description, it returns an error — an agent referenced as a
// sub-agent MUST carry a description (either inline on the grant or on the
// agent itself), since the parent model sees it as a tool function
// declaration and needs to know what it does.
func (c *Config) resolveSubAgentDescriptions() error {
	for i := range c.Agents {
		for j := range c.Agents[i].Tools {
			spec := &c.Agents[i].Tools[j]
			if spec.Agent == "" {
				continue
			}

			if spec.Description != "" {
				continue
			}

			child, err := c.FindAgent(spec.Agent)
			if err != nil {
				continue
			}

			if child.Description == "" {
				return fmt.Errorf(
					"agent %q grants sub-agent %q with no description: add a description field to the %q agents: entry",
					c.Agents[i].Name, spec.Agent, spec.Agent,
				)
			}

			spec.Description = child.Description
		}
	}

	return nil
}
