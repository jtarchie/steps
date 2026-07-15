// Package config parses and resolves a Concourse-style pipeline YAML file
// (resource_types/resources/jobs) and the config-merge logic (task and
// agent-invocation resolution) that both plan-time hashing and run-time
// execution share.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// artifactNamePattern constrains input/output/resource names used to build
// filesystem paths: this is load-bearing, not cosmetic — it rules out `..`
// segments and separators, so a name can never escape the directory it's
// joined under, and keeps the workspace copy/btrfs backends' shelled-out
// argv construction free of characters that would need escaping.
var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) //nolint:gochecknoglobals // static, read-only

// ValidateArtifactName checks name against artifactNamePattern. Used both at
// config-load time (validateArtifactNames) and by internal/workspace at
// runtime, when materializing a step's directories.
func ValidateArtifactName(name string) error {
	if !artifactNamePattern.MatchString(name) {
		return fmt.Errorf("invalid artifact name %q: must match %s", name, artifactNamePattern.String())
	}

	return nil
}

// Config is the top-level shape of a Concourse-style pipeline YAML file.
type Config struct {
	ResourceTypes []ResourceType `yaml:"resource_types"`
	Resources     []Resource     `yaml:"resources"`
	Agents        []Agent        `yaml:"agents"`
	Tasks         []Task         `yaml:"tasks"`
	Jobs          []Job          `yaml:"jobs"`
	// Workspace opts the pipeline into Concourse-style per-step isolation.
	// Absent (the default) keeps every step in a triggered build sharing one
	// mutable directory, exactly as before this field existed. See
	// WorkspaceConfig.
	Workspace *WorkspaceConfig `yaml:"workspace,omitempty"`
}

// WorkspaceConfig opts a pipeline into Concourse-style per-step workspace
// isolation: when set, task/agent/put steps materialize a directory built
// from their own declared inputs:/outputs: (see Step, Task) instead of
// sharing the build's directory with every other step. This is corruption
// hygiene, not a security sandbox — a step's shell commands can still reach
// outside the materialized directory via absolute paths, exactly as today.
type WorkspaceConfig struct {
	// Strategy is "copy" (portable; uses copy-on-write when the underlying
	// filesystem supports it — APFS clonefile on macOS, reflink on Linux —
	// and falls back to a plain recursive copy otherwise) or "btrfs" (Linux
	// only; instant copy-on-write via btrfs subvolume snapshots).
	Strategy string `yaml:"strategy"`
	// Root is where isolated build workspaces are materialized. Optional for
	// strategy: copy (defaults to the system temp directory); required for
	// strategy: btrfs, since the system temp directory (often tmpfs) is
	// commonly not itself a btrfs filesystem.
	Root string `yaml:"root,omitempty"`
	// Options holds strategy-specific tuning; currently btrfs only.
	Options WorkspaceOptions `yaml:"options,omitempty"`
}

// WorkspaceOptions holds strategy-specific workspace tuning.
type WorkspaceOptions struct {
	// Compression sets a btrfs subvolume's compression property: "zstd",
	// "lzo", "zlib", or "none". Valid only for strategy: btrfs.
	Compression string `yaml:"compression,omitempty"`
}

// ResourceType defines a resource kind as a set of shell command templates.
type ResourceType struct {
	Name string `yaml:"name"`
	// Image, when set, runs check/in/out in a fresh `docker run --rm`
	// container from this image instead of on the host. Empty (the default)
	// keeps host execution, byte-identical to before this field existed.
	Image  string             `yaml:"image,omitempty"`
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
	// Image, when set, runs this agent's run_shell/custom-tool commands in a
	// fresh `docker run --rm` container from this image instead of on the
	// host. A step's own Image, if set, overrides this for that step only
	// (see Step.Image). Empty (the default) keeps host execution.
	Image string `yaml:"image,omitempty"`
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
//
// StringToolChoice overrides whether forcing a required tool (see
// forceRequiredTool in internal/agent/conversation.go) uses OpenAI's named
// tool_choice object or the string "required" fallback. Left unset, it
// defaults to the resolved provider's requiresKey (cloud providers get the
// precise named form; local/no-auth providers like lmstudio/ollama, whose
// OpenAI-compat servers often don't support the object form, get the
// fallback) or false for an explicit endpoint: with no recognized provider
// prefix. A pointer so "unset" is distinguishable from an explicit false.
type AgentSource struct {
	Endpoint         string `yaml:"endpoint,omitempty"`
	Model            string `yaml:"model"`
	APIKeyEnv        string `yaml:"api_key_env,omitempty"`
	StringToolChoice *bool  `yaml:"string_tool_choice,omitempty"`
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
	// Required marks a custom tool's command as a resource-like action that
	// must succeed: a nonzero exit aborts the agent step (and, once attempts
	// are exhausted, the job) instead of being reported to the model as
	// {"error": ...} data for it to react to. Ignored on builtins.
	Required bool
}

// UnmarshalYAML decodes a ToolSpec from either a scalar (builtin name) or a
// mapping ({name, description, run, required}) YAML node.
func (t *ToolSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear in a decoded sequence element
	case yaml.ScalarNode:
		return value.Decode(&t.Builtin) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Run         string `yaml:"run"`
			Required    bool   `yaml:"required"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("agent tool: %w", err)
		}

		t.Name, t.Description, t.Run, t.Required = m.Name, m.Description, m.Run, m.Required

		return nil
	default:
		return fmt.Errorf("agent tool at line %d must be a builtin name or a {name, description, run} mapping", value.Line)
	}
}

// Task is a named, reusable command a `task` step can invoke by name instead
// of carrying its own `run:` inline. A step with `task: <name>` and no `run:`
// resolves against this list; a step's own `fix:`, if set, overrides the
// referenced task's Fix for that step. A step that does supply `run:` is
// always inline and never consults this list, even if a same-named entry
// exists here.
type Task struct {
	Name string   `yaml:"name"`
	Run  string   `yaml:"run"`
	Fix  *FixSpec `yaml:"fix,omitempty"`
	// Image, when set, runs this task's run: (and its verdict re-run) in a
	// fresh `docker run --rm` container from this image instead of on the
	// host. A referencing step's own Image, if non-empty, overrides this for
	// that step only — mirroring how Fix works. Empty (the default) keeps
	// host execution, byte-identical to before this field existed.
	Image string `yaml:"image,omitempty"`
	// Inputs/Outputs are consulted only when a pipeline sets workspace: (see
	// WorkspaceConfig); a referencing step's own Inputs/Outputs, if
	// non-nil, override these for that step only — mirroring how Fix works.
	Inputs  []string `yaml:"inputs,omitempty"`
	Outputs []string `yaml:"outputs,omitempty"`
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
	Version any `yaml:"version,omitempty"`
	// Task labels a task step. If Run is also set, the step is inline and
	// Run/Fix below are used as-is. If Run is empty, Task instead names a
	// tasks: entry (see Task) to resolve run/fix from; this step's own Fix,
	// if set, overrides the referenced task's Fix for this step only.
	Task string `yaml:"task,omitempty"`
	Run  string `yaml:"run,omitempty"`
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
	// Inputs/Outputs declare which named artifacts a task/agent/put step
	// draws from and (task/agent only) produces, when the pipeline sets
	// workspace: (see WorkspaceConfig). Each name is either a resource
	// fetched by an earlier get step or an output produced by an earlier
	// task/agent step. Omitted/nil means "none" for every step kind — put
	// steps have no implicit "all artifacts" view; declare inputs: for
	// whatever the out: command needs to see. Invalid without a top-level
	// workspace: block, and invalid on get steps; Outputs is additionally
	// invalid on put steps.
	Inputs  []string `yaml:"inputs,omitempty"`
	Outputs []string `yaml:"outputs,omitempty"`
	// Image, on a task or agent step, overrides the referenced task's/
	// agent's Image for this step only (inherit-only: a non-empty step Image
	// always wins, there is no way to force host execution from a step when
	// the task/agent sets one). Invalid on get/put steps — a put's execution
	// image comes from its resource type, and a get has no task/agent to
	// override.
	Image string `yaml:"image,omitempty"`
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

	err = cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	return &cfg, nil
}

// validate checks schema-level invariants that yaml.Unmarshal can't express
// on its own — in particular everything around workspace:/inputs:/outputs:,
// so a misconfigured pipeline fails at load time rather than mid-build.
func (c *Config) validate() error {
	err := c.validateWorkspace()
	if err != nil {
		return err
	}

	err = c.validateArtifactDecls()
	if err != nil {
		return err
	}

	return c.validateImages()
}

// validateImages rejects image: on get/put steps: a put's execution image
// comes from its resource type (ResourceType.Image), and a get step has no
// task/agent to scope.
func (c *Config) validateImages() error {
	for _, job := range c.Jobs {
		for i, step := range job.Plan {
			if step.Image == "" {
				continue
			}

			label := fmt.Sprintf("job %q step %d", job.Name, i)

			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): image is not valid on get steps", label, step.Get)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): image is not valid on put steps; set it on the resource_type instead", label, step.Put)
			}
		}
	}

	return nil
}

// UsesImages reports whether any resource_type, agent, task, or step sets
// image: — used to fail fast (before any step runs) when docker isn't
// available but the pipeline needs it.
func (c *Config) UsesImages() bool {
	for _, rt := range c.ResourceTypes {
		if rt.Image != "" {
			return true
		}
	}

	for _, a := range c.Agents {
		if a.Image != "" {
			return true
		}
	}

	for _, t := range c.Tasks {
		if t.Image != "" {
			return true
		}
	}

	for _, job := range c.Jobs {
		for _, step := range job.Plan {
			if step.Image != "" {
				return true
			}
		}
	}

	return false
}

var (
	workspaceStrategies = map[string]bool{"copy": true, "btrfs": true}
	compressionValues   = map[string]bool{"": true, "zstd": true, "lzo": true, "zlib": true, "none": true}
)

func (c *Config) validateWorkspace() error {
	ws := c.Workspace
	if ws == nil {
		return nil
	}

	if !workspaceStrategies[ws.Strategy] {
		return fmt.Errorf("workspace.strategy %q must be one of copy, btrfs", ws.Strategy)
	}

	if ws.Strategy == "btrfs" && ws.Root == "" {
		return errors.New("workspace.root is required for strategy: btrfs (the system temp directory is commonly not a btrfs filesystem)")
	}

	if !compressionValues[ws.Options.Compression] {
		return fmt.Errorf("workspace.options.compression %q must be one of zstd, lzo, zlib, none", ws.Options.Compression)
	}

	if ws.Options.Compression != "" && ws.Strategy != "btrfs" {
		return fmt.Errorf("workspace.options.compression is only valid for strategy: btrfs, not %q", ws.Strategy)
	}

	return nil
}

// declaresArtifacts reports whether a step or task carries any inputs:/
// outputs: at all — used to reject them outright when no workspace: block
// is configured.
func declaresArtifacts(inputs, outputs []string) bool {
	return inputs != nil || outputs != nil
}

func (c *Config) validateArtifactDecls() error {
	for i := range c.Tasks {
		task := c.Tasks[i]

		if c.Workspace == nil && declaresArtifacts(task.Inputs, task.Outputs) {
			return fmt.Errorf("task %q: inputs/outputs require a top-level workspace: block", task.Name)
		}

		err := validateArtifactNames(fmt.Sprintf("task %q", task.Name), task.Inputs, task.Outputs)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		for i, step := range job.Plan {
			err := c.validateStepArtifactDecls(job.Name, i, step)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Config) validateStepArtifactDecls(jobName string, i int, step Step) error {
	label := fmt.Sprintf("job %q step %d", jobName, i)

	if c.Workspace == nil && declaresArtifacts(step.Inputs, step.Outputs) {
		return fmt.Errorf("%s: inputs/outputs require a top-level workspace: block", label)
	}

	switch {
	case step.Get != "":
		if declaresArtifacts(step.Inputs, step.Outputs) {
			return fmt.Errorf("%s (get %q): inputs/outputs are not valid on get steps", label, step.Get)
		}

		return nil
	case step.Put != "":
		if step.Outputs != nil {
			return fmt.Errorf("%s (put %q): outputs are not valid on put steps", label, step.Put)
		}

		return validateArtifactNames(fmt.Sprintf("%s (put %q)", label, step.Put), step.Inputs, nil)
	default:
		return validateArtifactNames(label, step.Inputs, step.Outputs)
	}
}

// validateArtifactNames checks every name in inputs/outputs against
// artifactNamePattern (see workspace.go) and rejects duplicates within a
// list or a name appearing in both — in-place propagation (an output
// shadowing one of the same step's own inputs) isn't supported.
func validateArtifactNames(context string, inputs, outputs []string) error {
	seen := map[string]string{}

	check := func(names []string, kind string) error {
		for _, name := range names {
			err := ValidateArtifactName(name)
			if err != nil {
				return fmt.Errorf("%s: %w", context, err)
			}

			prevKind, ok := seen[name]
			if ok {
				if prevKind == kind {
					return fmt.Errorf("%s: duplicate %s %q", context, kind, name)
				}

				return fmt.Errorf("%s: %q cannot be both an input and an output of the same step", context, name)
			}

			seen[name] = kind
		}

		return nil
	}

	err := check(inputs, "input")
	if err != nil {
		return err
	}

	return check(outputs, "output")
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

// FindTask returns the task with the given name, or an error if not found.
func (c *Config) FindTask(name string) (*Task, error) {
	slog.Debug("task.find", "name", name)

	for i := range c.Tasks {
		if c.Tasks[i].Name == name {
			slog.Debug("task.find", "name", name, "found", true)

			return &c.Tasks[i], nil
		}
	}

	return nil, fmt.Errorf("no task named %q", name)
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

// ResolvedTask is a task step's run/fix, resolved against either the step's
// own inline fields or a tasks: entry it references by name. Both the merkle
// planner and the executor call ResolveTask so plan-time hashing and
// run-time execution stay in lockstep.
type ResolvedTask struct {
	Name    string
	Run     string
	Fix     *FixSpec
	Inputs  []string
	Outputs []string
	// Image, when non-empty, runs this task's run: (and any fix-loop
	// re-runs) in a container from this image instead of on the host. See
	// Task.Image/Step.Image.
	Image string
}

// ResolveTask resolves step into a ResolvedTask: a step carrying its own
// run: is inline and used as-is; otherwise step.Task names a tasks: entry,
// whose run/fix/inputs/outputs/image are used, except the step's own fix:,
// inputs:, outputs:, and image:, if set (non-nil/non-empty), which override
// the referenced task's for this step only — the same override idiom for
// all four.
func (c *Config) ResolveTask(step Step) (ResolvedTask, error) {
	if step.Run != "" {
		return ResolvedTask{
			Name: step.Task, Run: step.Run, Fix: step.Fix,
			Inputs: step.Inputs, Outputs: step.Outputs, Image: step.Image,
		}, nil
	}

	task, err := c.FindTask(step.Task)
	if err != nil {
		return ResolvedTask{}, fmt.Errorf("task %q: %w", step.Task, err)
	}

	fix := task.Fix
	if step.Fix != nil {
		fix = step.Fix
	}

	inputs := task.Inputs
	if step.Inputs != nil {
		inputs = step.Inputs
	}

	outputs := task.Outputs
	if step.Outputs != nil {
		outputs = step.Outputs
	}

	image := task.Image
	if step.Image != "" {
		image = step.Image
	}

	return ResolvedTask{Name: step.Task, Run: task.Run, Fix: fix, Inputs: inputs, Outputs: outputs, Image: image}, nil
}

// defaultMaxAgentTurns is the default cap on one attempt's tool-calling loop
// when an agent doesn't set max_turns. 3-6 round trips covers a typical
// review (list a dir, read a few files, run a command, respond); 8 leaves
// headroom while still bounding a runaway loop (a model that never stops
// requesting tools) to a small, predictable number of calls.
const defaultMaxAgentTurns = 8

// validReasoningEfforts are the only accepted values for an agent's
// reasoning_effort. The corresponding genai.ThinkingLevel mapping lives in
// internal/agent, which is the only package that needs the LLM-specific
// value side of this table.
var validReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true} //nolint:gochecknoglobals // static, read-only lookup table

// agentProvider is a built-in base URL + default API key env var for a
// model-name prefix like "openrouter/anthropic/claude-3.5-sonnet".
type agentProvider struct {
	baseURL     string
	keyEnv      string // default api_key_env for this provider; empty for local servers
	requiresKey bool
}

//nolint:gochecknoglobals // static, read-only lookup table
var agentProviders = map[string]agentProvider{
	"openai":     {"https://api.openai.com/v1/", "OPENAI_API_KEY", true},
	"openrouter": {"https://openrouter.ai/api/v1/", "OPENROUTER_API_KEY", true},
	"groq":       {"https://api.groq.com/openai/v1/", "GROQ_API_KEY", true},
	"together":   {"https://api.together.xyz/v1/", "TOGETHER_API_KEY", true},
	"lmstudio":   {"http://localhost:1234/v1/", "", false},
	"ollama":     {"http://localhost:11434/v1/", "", false},
}

// resolveAgentTarget interprets an optional "provider/" prefix on
// source.Model (e.g. "openrouter/anthropic/claude-3.5-sonnet") against
// agentProviders, splitting on the first "/" so a provider's own slashed
// model IDs survive intact. source.Endpoint/APIKeyEnv, when set, always
// override the derived values. A model with no recognized provider prefix
// requires an explicit source.Endpoint.
//
// stringOnlyToolChoice defaults to !provider.requiresKey for a recognized
// provider prefix (local/no-auth providers get the string-only tool_choice
// fallback; cloud providers get the precise named form) or false for an
// explicit endpoint:, and source.StringToolChoice, when set, always wins.
func resolveAgentTarget(source AgentSource) (baseURL, modelName, apiKeyEnv string, requiresKey, stringOnlyToolChoice bool, err error) {
	prefix, rest, hasPrefix := strings.Cut(source.Model, "/")

	provider, known := agentProviders[prefix]
	if hasPrefix && known && rest != "" {
		baseURL = source.Endpoint
		if baseURL == "" {
			baseURL = provider.baseURL
		}

		apiKeyEnv = source.APIKeyEnv
		if apiKeyEnv == "" {
			apiKeyEnv = provider.keyEnv
		}

		stringOnlyToolChoice = !provider.requiresKey
		if source.StringToolChoice != nil {
			stringOnlyToolChoice = *source.StringToolChoice
		}

		return ensureTrailingSlash(baseURL), rest, apiKeyEnv, provider.requiresKey || source.APIKeyEnv != "", stringOnlyToolChoice, nil
	}

	if source.Endpoint == "" {
		return "", "", "", false, false, fmt.Errorf("model %q has no known provider prefix; set source.endpoint", source.Model)
	}

	stringOnlyToolChoice = false
	if source.StringToolChoice != nil {
		stringOnlyToolChoice = *source.StringToolChoice
	}

	return ensureTrailingSlash(source.Endpoint), source.Model, source.APIKeyEnv, source.APIKeyEnv != "", stringOnlyToolChoice, nil
}

// ensureTrailingSlash normalizes a base URL to end in "/", since the
// OpenAI-compatible client resolves request paths (e.g. "chat/completions")
// relative to it.
func ensureTrailingSlash(rawURL string) string {
	if rawURL == "" || strings.HasSuffix(rawURL, "/") {
		return rawURL
	}

	return rawURL + "/"
}

// DefaultAgentToolSpecs is used when an agent grants no tools — the default
// is every built-in.
func DefaultAgentToolSpecs() []ToolSpec {
	return []ToolSpec{{Builtin: "read_file"}, {Builtin: "list_dir"}, {Builtin: "run_shell"}}
}

// ToolSpecName is the name a ToolSpec is referenced by: the builtin name for
// a builtin, or the custom tool's name.
func ToolSpecName(spec ToolSpec) string {
	if spec.Builtin != "" {
		return spec.Builtin
	}

	return spec.Name
}

// grantedToolIndex maps each tool an agent grants (by reference name) to its
// spec. An agent that grants nothing is treated as granting every built-in,
// so the simple "no tools: block" case still works.
func grantedToolIndex(agentTools []ToolSpec) map[string]ToolSpec {
	specs := agentTools
	if len(specs) == 0 {
		specs = DefaultAgentToolSpecs()
	}

	index := make(map[string]ToolSpec, len(specs))
	for _, spec := range specs {
		index[ToolSpecName(spec)] = spec
	}

	return index
}

// resolveEffectiveTools merges an agent's tool grant with a step's tool
// selection. An empty step selection inherits all of the agent's tools. A
// bare-name step entry must reference a tool the agent granted (built-ins,
// especially run_shell, are agent-gated — a step can't add one the agent
// withheld). An inline custom tool is always allowed: it is a specific,
// human-authored command, not a grant of arbitrary model capability.
func resolveEffectiveTools(agentTools, stepTools []ToolSpec) ([]ToolSpec, error) {
	if len(stepTools) == 0 {
		return agentTools, nil
	}

	granted := grantedToolIndex(agentTools)
	effective := make([]ToolSpec, 0, len(stepTools))

	for _, spec := range stepTools {
		if spec.Builtin != "" {
			grantedSpec, ok := granted[spec.Builtin]
			if !ok {
				return nil, fmt.Errorf("step selects tool %q, which the agent does not provide", spec.Builtin)
			}

			effective = append(effective, grantedSpec)

			continue
		}

		effective = append(effective, spec)
	}

	return effective, nil
}

// ResolvedInvocation is an agent + step reduced to everything needed to hash
// and run the step: the resolved connection, persona, dials, limits, and the
// effective (merged) tool set. ResolveAgentInvocation produces it for both
// planning (merkle hashing) and execution, so both compute identical hashes.
type ResolvedInvocation struct {
	AgentName   string
	BaseURL     string
	ModelName   string
	APIKeyEnv   string
	RequiresKey bool
	Persona     string
	// Generation dials, mirroring Agent's own fields once resolved. Kept flat
	// here (rather than a nested type) so this package doesn't need to depend
	// on anything LLM-client-specific — internal/agent assembles its own
	// request-config shape from these.
	Temperature     *float64
	TopP            *float64
	MaxTokens       int
	ReasoningEffort string // "", "low", "medium", or "high"
	MaxTurns        int
	Attempts        int
	ToolSpecs       []ToolSpec
	// StringOnlyToolChoice, when true, forces a required tool call (see
	// forceRequiredTool in internal/agent) via tool_choice: "required"
	// instead of a named function object — for providers whose
	// OpenAI-compat server rejects the object form. See resolveAgentTarget.
	StringOnlyToolChoice bool
	// Image, when non-empty, runs this step's run_shell/custom-tool commands
	// in a container from this image instead of on the host. See
	// Agent.Image/Step.Image.
	Image string
}

// ResolveAgentInvocation resolves the agent named by step against c,
// applying provider-prefix resolution, tool-grant merging, and defaulting
// (step.Attempts defaults to 1 — retries are a per-task concern, not part of
// the agent's config; agent.MaxTurns defaults to defaultMaxAgentTurns).
func (c *Config) ResolveAgentInvocation(step Step) (ResolvedInvocation, error) {
	agent, err := c.FindAgent(step.Agent)
	if err != nil {
		return ResolvedInvocation{}, err
	}

	baseURL, modelName, apiKeyEnv, requiresKey, stringOnlyToolChoice, err := resolveAgentTarget(agent.Source)
	if err != nil {
		return ResolvedInvocation{}, err
	}

	toolSpecs, err := resolveEffectiveTools(agent.Tools, step.Tools)
	if err != nil {
		return ResolvedInvocation{}, err
	}

	reasoning := strings.ToLower(agent.ReasoningEffort)
	if reasoning != "" && !validReasoningEfforts[reasoning] {
		return ResolvedInvocation{}, fmt.Errorf("reasoning_effort %q must be one of low, medium, high", agent.ReasoningEffort)
	}

	maxTurns := agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxAgentTurns
	}

	attempts := step.Attempts
	if attempts <= 0 {
		attempts = 1
	}

	image := agent.Image
	if step.Image != "" {
		image = step.Image
	}

	return ResolvedInvocation{
		AgentName:            agent.Name,
		BaseURL:              baseURL,
		ModelName:            modelName,
		APIKeyEnv:            apiKeyEnv,
		RequiresKey:          requiresKey,
		Persona:              agent.System,
		Temperature:          agent.Temperature,
		TopP:                 agent.TopP,
		MaxTokens:            agent.MaxTokens,
		ReasoningEffort:      reasoning,
		MaxTurns:             maxTurns,
		Attempts:             attempts,
		ToolSpecs:            toolSpecs,
		StringOnlyToolChoice: stringOnlyToolChoice,
		Image:                image,
	}, nil
}

// StableStrings returns a non-nil copy of names, so json.Marshal always
// encodes it as [] rather than null — a nil inputs/outputs list and an
// explicit inputs: [] must hash identically, since they mean the same thing
// (no artifacts). Used by merkle/agent content-hashing when folding in a
// step's Inputs/Outputs.
func StableStrings(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)

	return out
}
