package config

import (
	"embed"
	"fmt"
	"log/slog"
	"strings"
)

//go:embed prompts/*.md agents/*.yml tools/*.md skills/*/SKILL.md
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

// ReadBuiltinSkill returns the content of a named built-in skill.
// name is like "code-review" — resolves to "skills/<name>/SKILL.md".
func ReadBuiltinSkill(name string) (string, error) {
	data, err := builtinFS.ReadFile("skills/" + name + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("built-in skill %q not found", name)
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

// registerBuiltinAgents populates c.Agents with built-in agent profiles for
// any name not already defined by the user's pipeline config. A user-defined
// agent with the same name as a built-in overrides it (pipeline config wins).
func (c *Config) registerBuiltinAgents() {
	builtinNames, err := ListBuiltinAgentNames()
	if err != nil {
		slog.Warn("builtin.agents.list", "error", err)
		return
	}

	for _, name := range builtinNames {
		fullName := "@builtin/" + name
		if c.findAgentByName(fullName) {
			continue
		}

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
		c.Agents = append(c.Agents, agent)
		slog.Debug("builtin.agents.register", "name", fullName)
	}
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

// findAgentByName checks if an agent with the given name already exists.
func (c *Config) findAgentByName(name string) bool {
	for i := range c.Agents {
		if c.Agents[i].Name == name {
			return true
		}
	}
	return false
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
