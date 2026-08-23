package config

// The tasks: entry and its fix: agent.

import (
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"
)

// Task is a named, reusable command a `task` step can invoke by name instead
// of carrying its own `run:` inline. A step with `task: <name>` and no `run:`
// resolves against this list; a step's own `fix:`, if set, overrides the
// referenced task's Fix for that step. A step that does supply `run:` is
// always inline and never consults this list, even if a same-named entry
// exists here.
type Task struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	// File loads this task's run/fix/image/timeout/inputs/outputs from a YAML
	// document at a path relative to the pipeline file's directory (see
	// LoadConfig's resolveFileIncludes), so one task definition can be shared
	// across pipelines. Any field also set inline on this entry overrides the
	// loaded document's value for that field — the same "wins when set" idiom
	// ResolveTask already uses between a step and its tasks: entry. The
	// loaded document may not itself use file:/run_file:.
	File string `yaml:"file,omitempty"`
	// RunFile loads Run's text from a file at a path relative to the pipeline
	// file's directory, instead of writing it inline. Mutually exclusive with
	// Run.
	RunFile string   `yaml:"run_file,omitempty"`
	Fix     *FixSpec `yaml:"fix,omitempty"`
	// Image, when set, runs this task's run: (and its verdict re-run) in a
	// fresh `docker run --rm` container from this image instead of on the
	// host. A referencing step's own Image, if non-empty, overrides this for
	// that step only — mirroring how Fix works. Empty (the default) keeps
	// host execution, byte-identical to before this field existed.
	Image string `yaml:"image,omitempty"`
	// Env names host environment variables this task's run: is allowed to
	// see, on top of the always-allowed baseline (see shell.HostEnv). Names
	// only — see validateEnvValues. A referencing step's own Env, if
	// non-nil, overrides this for that step only, mirroring how Fix works.
	Env []string `yaml:"env,omitempty"`
	// User is the container user this task's run: executes as (docker's
	// --user). Empty takes the platform default — see shell's
	// defaultContainerUser. Only meaningful alongside Image.
	User string `yaml:"user,omitempty"`
	// Network is the container network this task's run: joins (docker's
	// --network); "none" cuts off egress entirely. Requires Image.
	Network string `yaml:"network,omitempty"`
	// Privileged runs this command's container with `docker run --privileged`.
	// Mirrors Concourse's privileged: (concourse-ci.org/docs/steps/task/).
	// Container-only, like Network — a host command has nothing to elevate,
	// so it is a load error without image:.
	Privileged bool `yaml:"privileged,omitempty"`
	// Limits caps the container's CPU and memory. Mirrors Concourse's
	// container_limits:; container-only for the same reason Privileged is.
	Limits *ContainerLimits `yaml:"container_limits,omitempty"`
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// (default) means no timeout. Inherited by task steps unless overridden.
	Timeout string `yaml:"timeout,omitempty"`
	// Inputs/Outputs are consulted only when a pipeline sets workspace: (see
	// WorkspaceConfig); a referencing step's own Inputs/Outputs, if
	// non-nil, override these for that step only — mirroring how Fix works.
	//
	// Inputs is the same *InputSpec a step carries, so `inputs:` means one
	// thing in the schema rather than two types that happened to share a
	// name. The list form decodes identically; only the scalar `all` differs,
	// and that stays put-only (validateTaskInputsAll) — a reusable task
	// declaring "everything available" would depend on whichever job called
	// it, which is the opposite of reusable.
	Inputs  *InputSpec `yaml:"inputs,omitempty"`
	Outputs []string   `yaml:"outputs,omitempty"`
}

// FixSpec is a task step's fix: — the agent to invoke when a run misses the
// step's own success criteria. A bare scalar names the agent (all other
// fields derived); a mapping allows per-task overrides. It implements
// yaml.Unmarshaler for the same scalar-or-mapping reason ToolSpec does.
//
// Those criteria are the step's Assert when it declares one and a nonzero
// exit otherwise — the SAME rule that grades the result, so a repair is never
// triggered by a different question than the one it is graded on. On a
// failure the named agent is invoked seeded with that verdict and the
// captured output, with the parent task auto-injected as a zero-arg rerun
// tool (its run:, never its fix:, so a rerun can't recurse); after it stops,
// the task's command is re-run and judged the same way. Fields left unset
// fall back to the agent's own defaults (Attempts to 1).
type FixSpec struct {
	Agent string // agents: entry to invoke on failure
	// Messages is what the repair agent is asked, one user turn per entry.
	// Empty uses a default fix prompt. Named for the step field it mirrors:
	// this is the same concept, and calling it something else here would make
	// the DSL say two words for one idea.
	Messages []string
	// MessageFiles loads Messages from files, one message per file, at paths
	// relative to the pipeline file's directory. Mutually exclusive with
	// Messages. Mapping form only (a bare scalar fix: has no room for it).
	MessageFiles []string
	Dir          string     // optional working dir, relative to the workspace
	Tools        []ToolSpec // optional subset/addition to the agent's tool grant
	// Attempts is how many times the fix agent's failing REQUEST is
	// re-issued. Unset takes the agent default; 0 is a load error, like
	// attempts: anywhere else — see dials.go.
	Attempts *int
	Timeout  string // optional wall-clock deadline per fix-agent attempt
}

// UnmarshalYAML decodes a FixSpec from either a scalar (agent name) or a
// mapping ({agent, prompt, message_files, dir, tools, attempts, timeout}) YAML
// node.
func (f *FixSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&f.Agent) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		err := rejectUnknownKeys(value, "task fix",
			"agent", "messages", "message_files", "dir", "tools", "attempts", "timeout")
		if err != nil {
			return err
		}

		var m struct {
			Agent        string     `yaml:"agent"`
			Messages     []string   `yaml:"messages"`
			MessageFiles []string   `yaml:"message_files"`
			Dir          string     `yaml:"dir"`
			Tools        []ToolSpec `yaml:"tools"`
			Attempts     *int       `yaml:"attempts"`
			Timeout      string     `yaml:"timeout"`
		}

		err = value.Decode(&m)
		if err != nil {
			return fmt.Errorf("task fix: %w", err)
		}

		f.Agent, f.Messages, f.MessageFiles, f.Dir = m.Agent, m.Messages, m.MessageFiles, m.Dir
		f.Tools, f.Attempts, f.Timeout = m.Tools, m.Attempts, m.Timeout

		return nil
	default:
		return fmt.Errorf("task fix at line %d must be an agent name or a {agent, prompt, ...} mapping", value.Line)
	}
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

	return nil, notFound("task", name, names(c.Tasks, func(t Task) string { return t.Name }))
}
