// Package main implements steps, a small CLI that interprets a
// Concourse-style pipeline YAML file (resource_types/resources/jobs):
// check discovers resource versions, get fetches one via a rendered
// shell command, and task runs a plan step's command.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level shape of a Concourse-style pipeline YAML file.
type Config struct {
	ResourceTypes []ResourceType `yaml:"resource_types"`
	Resources     []Resource     `yaml:"resources"`
	Agents        []Agent        `yaml:"agents"`
	Jobs          []Job          `yaml:"jobs"`
}

// ResourceType defines a resource kind as a set of shell command templates.
type ResourceType struct {
	Name   string             `yaml:"name"`
	Config ResourceTypeConfig `yaml:"config"`
}

// ResourceTypeConfig holds the check/in/out shell command templates.
// Templates may reference {{ source.x }} and (for in/out) {{ version.y }}.
type ResourceTypeConfig struct {
	Check string `yaml:"check"`
	In    string `yaml:"in"`
	Out   string `yaml:"out"`
}

// Resource is a named instance of a resource type, configured with a source.
type Resource struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Source map[string]any `yaml:"source"`
}

// Agent is a named, reusable worker an `agent` step invokes: it owns the
// model connection, persona, generation dials, limits, and the tool grant
// (the set of tools a step may draw from). A step supplies the per-task
// prompt, working directory, and tool selection.
type Agent struct {
	Name   string      `yaml:"name"`
	Source AgentSource `yaml:"source"`
	// System is the persona/system message given to the model. Empty falls
	// back to a generic CI-agent persona.
	System string `yaml:"system,omitempty"`
	// Generation dials, forwarded to the model when set. ReasoningEffort is
	// one of "low", "medium", "high" (for reasoning-capable models).
	Temperature     *float64 `yaml:"temperature,omitempty"`
	TopP            *float64 `yaml:"top_p,omitempty"`
	MaxTokens       int      `yaml:"max_tokens,omitempty"`
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty"`
	// MaxTurns caps the tool-calling loop (default maxAgentTurns). Retries
	// (attempts:) are a per-task concern and live on the step, not here.
	MaxTurns int `yaml:"max_turns,omitempty"`
	// Tools is the grant: the built-in tools this agent may use plus any
	// reusable custom tool definitions. A step selects a subset by name and
	// may add its own inline custom tools. Empty grants all built-ins.
	Tools []ToolSpec `yaml:"tools,omitempty"`
}

// AgentSource selects the model and how to reach it. Model may carry a
// provider prefix (e.g. "openrouter/anthropic/claude-3.5-sonnet",
// "lmstudio/qwen2.5-coder") that resolves Endpoint and a default APIKeyEnv
// from a built-in table (see resolveAgentTarget in agent.go); Endpoint, when
// set, is the API base URL (e.g. "https://api.openai.com/v1/") and overrides
// the derived one. APIKeyEnv names an OS environment variable read at run
// time — the key is never stored in YAML.
type AgentSource struct {
	Endpoint  string `yaml:"endpoint,omitempty"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
}

// ToolSpec is one entry in an agent step's tools: list — either a built-in
// tool referenced by name, or a custom command-backed tool. It implements
// yaml.Unmarshaler because a tools: list mixes bare scalar entries (builtin
// names) with mapping entries (custom tool definitions).
type ToolSpec struct {
	Builtin     string // set when the entry is a bare builtin name
	Name        string // custom tool: function name exposed to the model
	Description string // custom tool: description shown to the model
	Run         string // custom tool: sh -c template, {{ .args.X }} interpolated
}

// UnmarshalYAML decodes a ToolSpec from either a scalar (builtin name) or a
// mapping ({name, description, run}) YAML node.
func (t *ToolSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear in a decoded sequence element
	case yaml.ScalarNode:
		return value.Decode(&t.Builtin) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Run         string `yaml:"run"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("agent tool: %w", err)
		}

		t.Name, t.Description, t.Run = m.Name, m.Description, m.Run

		return nil
	default:
		return fmt.Errorf("agent tool at line %d must be a builtin name or a {name, description, run} mapping", value.Line)
	}
}

// FixSpec is a task step's fix: — the agent to invoke when the task's run:
// command exits nonzero. A bare scalar names the agent (all other fields
// derived); a mapping allows per-task overrides. It implements
// yaml.Unmarshaler for the same scalar-or-mapping reason ToolSpec does.
//
// On a task failure, the named agent is invoked seeded with the captured
// output, with the parent task auto-injected as a zero-arg rerun tool (its
// run:, never its fix:, so a rerun can't recurse); after it stops, the task's
// command is re-run and that exit code is the step's verdict. Fields left
// unset fall back to the agent's own defaults (Attempts to 1).
type FixSpec struct {
	Agent    string     // agents: entry to invoke on failure
	Prompt   string     // optional override; empty uses a default fix prompt
	Dir      string     // optional working dir, relative to the workspace
	Tools    []ToolSpec // optional subset/addition to the agent's tool grant
	Attempts int        // optional whole-conversation retry count (default 1)
}

// UnmarshalYAML decodes a FixSpec from either a scalar (agent name) or a
// mapping ({agent, prompt, dir, tools, attempts}) YAML node.
func (f *FixSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&f.Agent) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Agent    string     `yaml:"agent"`
			Prompt   string     `yaml:"prompt"`
			Dir      string     `yaml:"dir"`
			Tools    []ToolSpec `yaml:"tools"`
			Attempts int        `yaml:"attempts"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("task fix: %w", err)
		}

		f.Agent, f.Prompt, f.Dir, f.Tools, f.Attempts = m.Agent, m.Prompt, m.Dir, m.Tools, m.Attempts

		return nil
	default:
		return fmt.Errorf("task fix at line %d must be an agent name or a {agent, prompt, ...} mapping", value.Line)
	}
}

// Job is a named sequence of steps to run.
type Job struct {
	Name string `yaml:"name"`
	Plan []Step `yaml:"plan"`
}

// Step is a flat union of the step kinds this interpreter supports: get,
// task, put, and agent.
type Step struct {
	Get     string `yaml:"get,omitempty"`
	Trigger bool   `yaml:"trigger,omitempty"`
	// Version selects which version(s) a get step fetches: unset/"latest"
	// (default) picks the single latest version; "every" runs the rest of
	// the plan once per version returned by check; a map pins to a specific
	// version. Mirrors Concourse's get.version field.
	Version any    `yaml:"version,omitempty"`
	Task    string `yaml:"task,omitempty"`
	Run     string `yaml:"run,omitempty"`
	// Fix, on a task step, names an agent to invoke when run: exits nonzero:
	// the agent is seeded with the captured output and given the task itself
	// as a rerun tool, then the command is re-run to decide the step. A green
	// run never constructs the agent. See FixSpec.
	Fix *FixSpec `yaml:"fix,omitempty"`
	// Put names a resource to run its out command against; Params are
	// passed through to the out command as {{ params.x }}.
	Put    string         `yaml:"put,omitempty"`
	Params map[string]any `yaml:"params,omitempty"`
	// Agent names an agents: entry this step invokes. Prompt is the task
	// given to the model (not templated — freeform text is likely to contain
	// literal {{ }} that isn't meant as a template). Dir is the step's
	// working directory relative to the job's workspace, since there's no
	// run: string to embed a cd in. Tools selects a subset of the agent's
	// granted tools by name and may add inline custom tools for this task
	// (empty means all of the agent's tools). Attempts overrides the agent's
	// default retry count (attempts: 3 = up to 3 total tries, including the
	// first); 0/unset inherits the agent's default (which itself defaults
	// to 1).
	Agent    string     `yaml:"agent,omitempty"`
	Prompt   string     `yaml:"prompt,omitempty"`
	Dir      string     `yaml:"dir,omitempty"`
	Tools    []ToolSpec `yaml:"tools,omitempty"`
	Attempts int        `yaml:"attempts,omitempty"`
}

// LoadConfig reads and parses a pipeline YAML file at path.
func LoadConfig(path string) (*Config, error) {
	slog.Debug("config.load", "path", path)

	data, err := os.ReadFile(path) //nolint:gosec // path is the pipeline file the user asked to run, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file %q: %w", path, err)
	}

	var cfg Config

	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse pipeline YAML %q: %w", path, err)
	}

	slog.Info("config.loaded",
		"path", path,
		"resource_types", len(cfg.ResourceTypes),
		"resources", len(cfg.Resources),
		"jobs", len(cfg.Jobs),
	)

	return &cfg, nil
}

// FindResource returns the resource with the given name, or an error if not found.
func (c *Config) FindResource(name string) (*Resource, error) {
	slog.Debug("resource.find", "name", name)

	for i := range c.Resources {
		if c.Resources[i].Name == name {
			slog.Debug("resource.find", "name", name, "type", c.Resources[i].Type, "found", true)

			return &c.Resources[i], nil
		}
	}

	return nil, fmt.Errorf("no resource named %q", name)
}

// FindResourceType returns the resource type with the given name, or an error if not found.
func (c *Config) FindResourceType(name string) (*ResourceType, error) {
	slog.Debug("resource_type.find", "name", name)

	for i := range c.ResourceTypes {
		if c.ResourceTypes[i].Name == name {
			slog.Debug("resource_type.find", "name", name, "found", true)

			return &c.ResourceTypes[i], nil
		}
	}

	return nil, fmt.Errorf("no resource_type named %q", name)
}

// FindAgent returns the agent with the given name, or an error if not found.
func (c *Config) FindAgent(name string) (*Agent, error) {
	slog.Debug("agent.find", "name", name)

	for i := range c.Agents {
		if c.Agents[i].Name == name {
			slog.Debug("agent.find", "name", name, "found", true)

			return &c.Agents[i], nil
		}
	}

	return nil, fmt.Errorf("no agent named %q", name)
}

// FindJob returns the job with the given name, or an error if not found.
func (c *Config) FindJob(name string) (*Job, error) {
	slog.Debug("job.find", "name", name)

	for i := range c.Jobs {
		if c.Jobs[i].Name == name {
			slog.Debug("job.find", "name", name, "steps", len(c.Jobs[i].Plan), "found", true)

			return &c.Jobs[i], nil
		}
	}

	return nil, fmt.Errorf("no job named %q (available: %v)", name, c.JobNames())
}

// JobNames returns the names of every job in the pipeline, in declaration
// order, for use in "which job?" error messages.
func (c *Config) JobNames() []string {
	names := make([]string, 0, len(c.Jobs))
	for _, j := range c.Jobs {
		names = append(names, j.Name)
	}

	return names
}
