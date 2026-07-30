// Package config parses and resolves a Concourse-style pipeline YAML file
// (resource_types/resources/jobs) and the config-merge logic (task and
// agent-invocation resolution) that both plan-time hashing and run-time
// execution share.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
	MCPServers    []MCPServer    `yaml:"mcp_servers,omitempty"`
	Tasks         []Task         `yaml:"tasks"`
	Jobs          []Job          `yaml:"jobs"`
	// Workspace opts the pipeline into Concourse-style per-step isolation.
	// Absent (the default) keeps every step in a triggered build sharing one
	// mutable directory, exactly as before this field existed. See
	// WorkspaceConfig.
	Workspace *WorkspaceConfig `yaml:"workspace,omitempty"`
	// Assert, at the top level, names the ordered set of job names that
	// `steps test` must have run (see Assert). It's a self-verification
	// meta-check, never hashed.
	Assert *Assert `yaml:"assert,omitempty"`
}

// Assert is a self-verification directive, in one of two shapes depending on
// where it's attached:
//   - On a Config (top level) or a Job, only Execution is valid: an ordered
//     list of the names that must have run — job names for a Config, task/
//     agent/hook names for a Job. By omission it also asserts what must NOT
//     run. A matching Job assert clears the plan's failure, so one green
//     fixture can contain deliberately-failing tasks.
//   - On a task/agent Step, only Stdout/Code are valid: Stdout is a substring
//     the step's captured output must contain, Code the exact expected exit
//     code (task only). A matching assert makes a non-zero-exit task a
//     success.
type Assert struct {
	Execution []string `yaml:"execution,omitempty"`
	Stdout    *string  `yaml:"stdout,omitempty"`
	Code      *int     `yaml:"code,omitempty"`
	// ToolCalls, on an agent step, asserts the ordered trajectory of tool
	// calls the model made (see ExpectedToolCall). Agent-only: a task step
	// runs no tools. Every entry must appear, in order, as a subsequence of
	// the observed calls.
	ToolCalls []ExpectedToolCall `yaml:"tool_calls,omitempty"`
}

// ExpectedToolCall is one entry in an agent step's assert.tool_calls: the
// tool's name, plus (optionally) a subset of the arguments the model must
// have called it with. Args is a subset match — every listed key must be
// present with an equal value, and any extra actual argument is ignored.
// Values compare as strings, since every argument reaching a tool's run:
// template is rendered as one (this is a deliberate divergence from
// secret-agent's eval matcher, which coerces across int/float).
type ExpectedToolCall struct {
	Name string            `yaml:"name"`
	Args map[string]string `yaml:"args,omitempty"`
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
//
// MCP, when set, is mutually exclusive with Check/In/Out: this resource
// type's check/in/out are calls to a configured mcp_servers: entry instead
// of shell commands (see MCPResourceConfig, validateResourceTypeConfig).
type ResourceTypeConfig struct {
	Check string             `yaml:"check,omitempty"`
	In    string             `yaml:"in,omitempty"`
	Out   string             `yaml:"out,omitempty"`
	MCP   *MCPResourceConfig `yaml:"mcp,omitempty"`
}

// MCPResourceConfig backs a resource type's check/in/out with calls to a
// named mcp_servers: entry instead of shell commands. Check is required (a
// type with no way to discover versions is useless); In and Out are
// optional — see resource.CheckVersions/RunIn/RunOut's mcp*/ branches for
// exactly what arguments each call receives and how its result is used.
type MCPResourceConfig struct {
	Server string       `yaml:"server"`
	Check  *MCPToolCall `yaml:"check,omitempty"`
	In     *MCPToolCall `yaml:"in,omitempty"`
	Out    *MCPToolCall `yaml:"out,omitempty"`
}

// MCPToolCall names the remote tool a resource-type lifecycle stage calls.
type MCPToolCall struct {
	Tool string `yaml:"tool"`
}

// MCPServer is a reusable, named MCP server connection: configured once
// under mcp_servers: and shared across any number of agents: tool grants
// and resource_types: mcp: backends — the same once-configured/many-
// consumers idiom as Agent/Resource. Two transports are supported: HTTP
// (Streamable HTTP, via Endpoint) or stdio (a local subprocess, via Command/
// Args/Cwd) — exactly one of Endpoint/Command is set (see
// validateMCPServerTransport). A stdio server has no request to attach
// credentials to, so Auth must be unset ("none") when Command is set.
type MCPServer struct {
	Name     string   `yaml:"name"`
	Endpoint string   `yaml:"endpoint,omitempty"`
	Command  string   `yaml:"command,omitempty"`
	Args     []string `yaml:"args,omitempty"`
	// Cwd is the working directory a stdio server's subprocess is spawned
	// in. An ABSOLUTE path is used verbatim — a fixed location on the host,
	// resolved identically for every step. A RELATIVE path is resolved
	// against the agent step's own working directory (see
	// WithResolvedMCPCwd), which is what lets a server be pointed at an
	// input artifact — `cwd: repo` for a language server that must index the
	// same materialized tree the agent's file tools read. Empty inherits the
	// steps process's own cwd.
	//
	// Relative only makes sense where a step workspace exists, so it is
	// rejected for a server backing a resource type's mcp: config — a
	// check/in/out runs with no agent step to resolve against.
	Cwd  string        `yaml:"cwd,omitempty"`
	Auth MCPServerAuth `yaml:"auth,omitempty"`
}

// WithResolvedMCPCwd returns cfg with every stdio server's relative Cwd
// joined against baseDir — the working directory of the agent step whose
// tools are about to be built. cfg is returned unchanged when no server
// needs it, so the overwhelmingly common case (no mcp_servers:, or all
// absolute) allocates nothing and hashes identically.
//
// The copy is shallow but the MCPServers slice is fresh, so a step
// resolving its own view can never mutate the shared config another step
// (with a different working directory) will resolve later.
func WithResolvedMCPCwd(cfg *Config, baseDir string) *Config {
	if cfg == nil || baseDir == "" {
		return cfg
	}

	needed := false

	for _, srv := range cfg.MCPServers {
		if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
			needed = true

			break
		}
	}

	if !needed {
		return cfg
	}

	servers := make([]MCPServer, len(cfg.MCPServers))
	copy(servers, cfg.MCPServers)

	for i, srv := range servers {
		if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
			servers[i].Cwd = filepath.Join(baseDir, srv.Cwd)
		}
	}

	resolved := *cfg
	resolved.MCPServers = servers

	return &resolved
}

// IsStdio reports whether srv is a stdio (local subprocess) server rather
// than an HTTP one. Exactly one of Endpoint/Command is set — see
// validateMCPServerTransport.
func (s MCPServer) IsStdio() bool {
	return s.Command != ""
}

// MCPServerAuth selects how steps authenticates to an MCP server. Type is
// "none" (default, when Auth is omitted entirely), "bearer" (a static token
// read from an OS environment variable named by APIKeyEnv — mirrors
// AgentSource.APIKeyEnv exactly: the credential is never stored in YAML),
// or "oauth" (interactive authorization-code + PKCE via `steps mcp login`,
// with silent refresh at run/watch time — see internal/mcp).
type MCPServerAuth struct {
	Type      string   `yaml:"type"`
	APIKeyEnv string   `yaml:"api_key_env,omitempty"`
	Scopes    []string `yaml:"scopes,omitempty"`
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
	// Description is the human-readable summary of what this agent does. It is
	// shown to the model when another agent grants this one as a sub-agent tool
	// (see ToolSpec.Agent) with no inline description. An agent referenced as a
	// sub-agent MUST have a description — either inline on the grant or on the
	// agent itself — or pipeline load fails with a clear error.
	Description string `yaml:"description,omitempty"`
	// File loads this agent's source/image/system/dials/tools from a YAML
	// document at a path relative to the pipeline file's directory (see
	// LoadConfig's resolveFileIncludes), so one agent definition can be
	// shared across pipelines. Any field also set inline on this entry
	// overrides the loaded document's value for that field — the same "wins
	// when set" idiom ResolveTask already uses between a step and its tasks:
	// entry. The loaded document may not itself use file:/system_file:.
	File string `yaml:"file,omitempty"`
	// Image, when set, runs this agent's run_shell/custom-tool commands in a
	// fresh `docker run --rm` container from this image instead of on the
	// host. A step's own Image, if set, overrides this for that step only
	// (see Step.Image). Empty (the default) keeps host execution.
	Image string `yaml:"image,omitempty"`
	// System is the persona/system message given to the model. Empty falls
	// back to a generic CI-agent persona.
	System string `yaml:"system,omitempty"`
	// SystemFile loads System's text from a file at a path relative to the
	// pipeline file's directory, instead of writing it inline — useful since a
	// persona is often long freeform prose. Mutually exclusive with System.
	SystemFile string `yaml:"system_file,omitempty"`
	// Generation dials, forwarded to the model when set. ReasoningEffort is
	// one of "low", "medium", "high" (for reasoning-capable models).
	Temperature     *float64 `yaml:"temperature,omitempty"`
	TopP            *float64 `yaml:"top_p,omitempty"`
	MaxTokens       int      `yaml:"max_tokens,omitempty"`
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty"`
	// MaxTurns caps the tool-calling loop (default maxAgentTurns). Retries
	// (attempts:) are a per-task concern and live on the step, not here.
	MaxTurns int `yaml:"max_turns,omitempty"`
	// CompactAfterTokens caps how large a conversation's estimated token
	// count grows before older turns are summarized away and replaced by a
	// running summary (see internal/agent/compaction.go and docs/agents.md's
	// "Compacting long conversations" section). A pointer, not a plain int,
	// because unset and explicit 0 mean different things: unset resolves to
	// defaultCompactAfterTokens (compaction ON by default), while 0 disables
	// compaction entirely — the same distinction Temperature/TopP already
	// need a pointer for. Like MaxTurns, this is agent-only: no per-step
	// override.
	CompactAfterTokens *int `yaml:"compact_after_tokens,omitempty"`
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

// ToolSpec is one entry in an agent step's tools: list — a built-in tool
// referenced by name, a custom command-backed tool, or a sub-agent tool (an
// agents: entry exposed to the model as a callable tool, see Agent below). It
// implements yaml.Unmarshaler because a tools: list mixes bare scalar entries
// (builtin names) with mapping entries (custom tool / sub-agent definitions).
type ToolSpec struct {
	Builtin     string // set when the entry is a bare builtin name
	Name        string // custom tool: function name exposed to the model
	Description string // custom tool (and sub-agent tool): description shown to the model
	Run         string // custom tool: sh -c template, {{ .args.X }} interpolated
	// Agent, when set, makes this tool a sub-agent delegation: the named
	// agents: entry is exposed to the model as a callable tool taking a single
	// `request` string; each call runs that agent's own fresh tool-calling
	// conversation (its own model/persona/dials/tool grant) in the caller's
	// working directory, and its final text answer is the tool result. A
	// sub-agent tool sets no builtin/name/run and can never be required:
	// (enforced at LoadConfig by validateAgentGraph). Sub-agents may
	// themselves grant sub-agents, up to maxSubAgentDepth, with no cycles.
	Agent string
	// MCP, when set, makes this a passthrough to one or more tools on a
	// configured mcp_servers: entry (MCP names the server). Three forms:
	//   - MCPTool set ({mcp, tool} in YAML): grants that one remote tool;
	//     only this form may also set Description/Required/MaxCalls.
	//   - MCPTools set ({mcp, tools: [...]} in YAML): grants that named
	//     subset, each keeping its own server-advertised description.
	//   - neither set ({mcp} alone): grants every tool the server exposes.
	// Like a sub-agent tool, an MCP grant must live on the agents: entry
	// (or a fix:'s own tools:), never introduced inline on a step
	// (enforced by validateMCPToolGrants/resolveEffectiveTools), and Args
	// is invalid on any form — arguments are schema-shaped by the remote
	// server, not a flat string template.
	MCP      string
	MCPTool  string
	MCPTools []string
	// Required marks a custom tool's command as a resource-like action that
	// must succeed: a nonzero exit aborts the agent step (and, once attempts
	// are exhausted, the job) instead of being reported to the model as
	// {"error": ...} data for it to react to. Ignored on builtins; invalid on
	// sub-agent tools.
	Required bool
	// MaxCalls caps how many times this custom tool may be invoked within one
	// attempt's conversation (0/unset = unlimited). The (N+1)th call is
	// rejected as ordinary tool-result data ({"error": "... call budget
	// exhausted ..."}), never an aborted attempt. The counter resets on an
	// attempts: restart (a fresh conversation is a fresh budget). Invalid on
	// builtins and sub-agent tools.
	MaxCalls int
	// Args pins argument values a custom tool's run: template may reference:
	// merged OVER the model's own arguments at call time (pinned always
	// wins), and excluded from the parameter schema shown to the model, so
	// the model can neither see nor override them — "the model chooses when,
	// the machine chooses where." Values are plain strings; not templated.
	// Invalid on builtins and sub-agent tools.
	Args map[string]string
	// MaxOutputBytes lowers the inline output budget for this one tool
	// (0/unset = the global maxToolOutputBytes). It can only ever NARROW the
	// cap, never widen it — a value at or above the global cap resolves back
	// to the global cap.
	//
	// It exists for a tool whose output is known to be mostly noise: a fuzzy
	// search that returns a ranked list where the answer is the first few
	// entries, for instance, where the tail costs context on every
	// subsequent turn and buys nothing. Narrowing loses no data — the
	// overflow still spills to a file the model can read back — it only
	// shrinks what lands in the conversation.
	//
	// Deliberately NOT valid on a built-in: those carry their own output
	// contract (read_file pages, list_dir counts entries, search_files is
	// bounded by arithmetic), and a second, conflicting bound on top of a
	// designed one is a bug surface rather than a knob. Also invalid on a
	// sub-agent tool, whose result is another agent's considered answer, not
	// a data dump. Valid on custom tools and on all three MCP grant forms.
	MaxOutputBytes int
}

// UnmarshalYAML decodes a ToolSpec from either a scalar (builtin name) or a
// mapping YAML node: {name, description, run, required, max_calls, args} for
// a custom tool, {agent, description} for a sub-agent tool, {mcp, tool,
// description, required, max_calls} / {mcp, tools: [...]} / {mcp} for an MCP
// tool grant (see ToolSpec.MCP), or {builtin, description} to reference a
// builtin by mapping instead of a bare scalar — the only reason to do so is
// to hit a validation error like max_calls/args on a builtin explicitly
// (validateToolCallGuardShape), since a bare scalar entry has no room for
// those fields at all.
func (t *ToolSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear in a decoded sequence element
	case yaml.ScalarNode:
		return value.Decode(&t.Builtin) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Builtin        string            `yaml:"builtin"`
			Name           string            `yaml:"name"`
			Description    string            `yaml:"description"`
			Run            string            `yaml:"run"`
			Agent          string            `yaml:"agent"`
			MCP            string            `yaml:"mcp"`
			Tool           string            `yaml:"tool"`
			Tools          []string          `yaml:"tools"`
			Required       bool              `yaml:"required"`
			MaxCalls       int               `yaml:"max_calls"`
			MaxOutputBytes int               `yaml:"max_output_bytes"`
			Args           map[string]string `yaml:"args"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("agent tool: %w", err)
		}

		t.Builtin, t.Name, t.Description, t.Run, t.Agent, t.Required = m.Builtin, m.Name, m.Description, m.Run, m.Agent, m.Required
		t.MaxCalls, t.Args, t.MaxOutputBytes = m.MaxCalls, m.Args, m.MaxOutputBytes
		t.MCP, t.MCPTool, t.MCPTools = m.MCP, m.Tool, m.Tools

		return nil
	default:
		return fmt.Errorf("agent tool at line %d must be a builtin name or a {name, description, run} / {agent, description} mapping", value.Line)
	}
}

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
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// (default) means no timeout. Inherited by task steps unless overridden.
	Timeout string `yaml:"timeout,omitempty"`
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
	Agent  string // agents: entry to invoke on failure
	Prompt string // optional override; empty uses a default fix prompt
	// PromptFile loads Prompt's text from a file at a path relative to the
	// pipeline file's directory, instead of writing it inline. Mutually
	// exclusive with Prompt. Mapping form only (a bare scalar fix: has no
	// room for it).
	PromptFile string
	Dir        string     // optional working dir, relative to the workspace
	Tools      []ToolSpec // optional subset/addition to the agent's tool grant
	Attempts   int        // optional whole-conversation retry count (default 1)
	Timeout    string     // optional wall-clock deadline per fix-agent attempt
}

// UnmarshalYAML decodes a FixSpec from either a scalar (agent name) or a
// mapping ({agent, prompt, prompt_file, dir, tools, attempts, timeout}) YAML
// node.
func (f *FixSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&f.Agent) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Agent      string     `yaml:"agent"`
			Prompt     string     `yaml:"prompt"`
			PromptFile string     `yaml:"prompt_file"`
			Dir        string     `yaml:"dir"`
			Tools      []ToolSpec `yaml:"tools"`
			Attempts   int        `yaml:"attempts"`
			Timeout    string     `yaml:"timeout"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("task fix: %w", err)
		}

		f.Agent, f.Prompt, f.PromptFile, f.Dir = m.Agent, m.Prompt, m.PromptFile, m.Dir
		f.Tools, f.Attempts, f.Timeout = m.Tools, m.Attempts, m.Timeout

		return nil
	default:
		return fmt.Errorf("task fix at line %d must be an agent name or a {agent, prompt, ...} mapping", value.Line)
	}
}

// WhenSpec is a step's when: guard — an explicit shell command whose EXIT
// CODE decides whether the step runs at all: 0 runs it, nonzero skips it. A
// nonzero exit is a legitimate "false" (a `grep -q` that finds nothing), never
// a failure; only a runner-level error (the command could not be started at
// all — a bad image, a docker daemon that isn't up) fails the step, so an
// infrastructure problem is never silently read as "skip".
//
// A skipped step behaves exactly like a merkle-cached skip: it fires no hooks,
// records no node or job_run, and does not appear in a job's assert.execution
// log. The guard runs in the same view the step itself would get — under the
// step's resolved image, in a directory materialized from the step's declared
// inputs — so it can read what the step reads.
//
// It implements yaml.Unmarshaler for the same scalar-or-mapping reason
// FixSpec does: `when: test -f x` is the common case, `when: {run: ...}` the
// explicit one.
type WhenSpec struct {
	Run string
}

// UnmarshalYAML decodes a WhenSpec from either a scalar (the command) or a
// mapping ({run}) YAML node.
func (w *WhenSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&w.Run) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Run string `yaml:"run"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step when: %w", err)
		}

		w.Run = m.Run

		return nil
	default:
		return fmt.Errorf("step when at line %d must be a command string or a {run} mapping", value.Line)
	}
}

// Hooks is the Concourse-style hook set a Step or a Job can carry. Each hook
// is itself a full Step restricted to task/put/agent kinds (get is rejected at
// LoadConfig time); a hook may recursively carry its own Hooks. on_success
// runs after a green outcome, on_failure/on_error/on_abort after the matching
// failure classification, and ensure always runs last regardless of outcome.
type Hooks struct {
	OnSuccess *Step `yaml:"on_success,omitempty"`
	OnFailure *Step `yaml:"on_failure,omitempty"`
	OnError   *Step `yaml:"on_error,omitempty"`
	OnAbort   *Step `yaml:"on_abort,omitempty"`
	Ensure    *Step `yaml:"ensure,omitempty"`
}

// Empty reports whether no hook is set.
func (h Hooks) Empty() bool {
	return h.OnSuccess == nil && h.OnFailure == nil && h.OnError == nil && h.OnAbort == nil && h.Ensure == nil
}

// Each calls fn for every non-nil hook, in a fixed order (on_success,
// on_failure, on_error, on_abort, ensure), passing the hook's YAML name.
func (h Hooks) Each(fn func(name string, step *Step) error) error {
	pairs := []struct {
		name string
		step *Step
	}{
		{"on_success", h.OnSuccess},
		{"on_failure", h.OnFailure},
		{"on_error", h.OnError},
		{"on_abort", h.OnAbort},
		{"ensure", h.Ensure},
	}

	for _, p := range pairs {
		if p.step == nil {
			continue
		}

		err := fn(p.name, p.step)
		if err != nil {
			return err
		}
	}

	return nil
}

// Job is a named sequence of steps to run.
type Job struct {
	Name  string `yaml:"name"`
	Plan  []Step `yaml:"plan"`
	Hooks Hooks  `yaml:",inline"`
	// Assert, on a job, names the ordered set of task/agent/hook names the
	// job's run must have produced (see Assert). A match clears the plan's
	// failure; a mismatch fails the job. Never hashed.
	Assert *Assert `yaml:"assert,omitempty"`
}

// Step is a flat union of the step kinds this interpreter supports: get,
// task, put, and agent.
type Step struct {
	Get     string `yaml:"get,omitempty"`
	Trigger bool   `yaml:"trigger,omitempty"`
	// Version selects which version(s) a get step fetches: unset/"latest"
	// (default) picks the single latest version; "every" runs the rest of
	// the plan once per version returned by check; a map pins to a specific
	// version. Mirrors Concourse's get.version field, including that a
	// failed version's build does not stop "every" from attempting the
	// remaining versions — see docs/conformance.md and
	// internal/pipeline/conformance_test.go's
	// TestConformanceGetVersionEveryContinuesPastFailure.
	Version any `yaml:"version,omitempty"`
	// Task labels a task step. If Run is also set, the step is inline and
	// Run/Fix below are used as-is. If Run is empty, Task instead names a
	// tasks: entry (see Task) to resolve run/fix from; this step's own Fix,
	// if set, overrides the referenced task's Fix for this step only.
	Task string `yaml:"task,omitempty"`
	Run  string `yaml:"run,omitempty"`
	// RunFile loads Run's text from a file at a path relative to the pipeline
	// file's directory, instead of writing it inline (see LoadConfig's
	// resolveFileIncludes). Mutually exclusive with Run. Task steps only.
	RunFile string `yaml:"run_file,omitempty"`
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
	Agent  string `yaml:"agent,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
	// PromptFile supplies Prompt from a file instead of inline text, in one of
	// two forms (see FileRef): a scalar path, relative to the pipeline file's
	// directory, resolved at load time exactly like RunFile/SystemFile; or a
	// {artifact, path} mapping naming a file inside an artifact this step
	// declares in its inputs:, read at run time once that artifact is
	// fetched. The run-time form exists only here (not for a task's run:, not
	// for an agent's own definition/persona) — see docs/agents.md. It costs
	// no merkle caching: an agent step's chain is already unconditionally
	// unskippable (see internal/merkle's planNonGetNode), so there is nothing
	// to lose by resolving this after plan time. Mutually exclusive with
	// Prompt in the scalar form.
	PromptFile *FileRef   `yaml:"prompt_file,omitempty"`
	Dir        string     `yaml:"dir,omitempty"`
	Tools      []ToolSpec `yaml:"tools,omitempty"`
	Attempts   int        `yaml:"attempts,omitempty"`
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// (default) means no timeout. Valid on all step kinds (a get step's
	// timeout bounds both its check and in commands); for task/put steps it
	// overrides the referenced task/agent's Timeout.
	Timeout string `yaml:"timeout,omitempty"`
	// Inputs/Outputs declare which named artifacts a task/agent/put step
	// draws from and (task/agent only) produces. Each name is either a
	// resource fetched by an earlier get step or an output produced by an
	// earlier task/agent step. Both are optional and default to empty: an
	// absent inputs: means "sees nothing declared" (there is no requirement to
	// declare them). When present, an inputs: naming an artifact nothing
	// produces is caught by workspace.ValidateArtifactFlow. Declarations only
	// change what a step physically sees under a top-level workspace: block
	// (see WorkspaceConfig); without one they are a validated contract, not
	// isolation, and are never folded into a node's hash (see internal/merkle).
	// Inputs is a *InputSpec so an absent key (nil) is distinguishable from an
	// explicit empty list — which matters for ResolveTask's override rule (a
	// step's inputs: override its tasks: entry only when declared) — and so put
	// steps can accept the scalar `inputs: all` (every available artifact) in
	// addition to a sequence of names. Invalid on get steps; Outputs is
	// additionally invalid on put steps.
	Inputs  *InputSpec `yaml:"inputs,omitempty"`
	Outputs []string   `yaml:"outputs,omitempty"`
	// Image, on a task or agent step, overrides the referenced task's/
	// agent's Image for this step only (inherit-only: a non-empty step Image
	// always wins, there is no way to force host execution from a step when
	// the task/agent sets one). Invalid on get/put steps — a put's execution
	// image comes from its resource type, and a get has no task/agent to
	// override.
	Image string `yaml:"image,omitempty"`
	// When, on a task/put/agent step, gates whether the step runs at all: an
	// explicit command whose exit code decides (0 runs, nonzero skips). See
	// WhenSpec. Invalid on get steps — a get fans the remainder of the plan
	// out per version, so a conditional get has no coherent meaning.
	When *WhenSpec `yaml:"when,omitempty"`
	// To, on a task/put/agent step, routes to another step in the SAME
	// get-segment based on this step's outcome, keyed by outcome name:
	// "success"/"failure" for a task/put/verdict-less agent, or a verdict name
	// for an agent that declares Verdicts. An open map (not a fixed struct) so
	// verdict keys — and, later, exit-code keys — need no new type. "success"
	// and "failure" are reserved keys. Invalid on get steps and hook steps.
	// Absent, the plan falls through in declaration order exactly as before.
	// See validateStepTransitions and internal/pipeline's resolveTransition.
	To map[string]string `yaml:"to,omitempty"`
	// MaxVisits caps how many times THIS step may execute in one run. It is
	// required (LoadConfig) whenever any To target routes backward (a target
	// at or before this step's own position within its segment); 0/unset means
	// unbounded, which is only legal when every To target is strictly forward.
	MaxVisits int `yaml:"max_visits,omitempty"`
	// Verdicts, on an agent step, declares the outcome vocabulary the model
	// emits. Its presence turns on verdict mode: internal/agent synthesizes a
	// required `verdict` tool whose enum is exactly these, the model must call
	// it, and To routes on the chosen value. Every declared verdict must have a
	// To entry, and no verdict may be named with a reserved key. Agent-only.
	Verdicts []string `yaml:"verdicts,omitempty"`
	// ContextPaths lists files whose contents are injected at conversation
	// start as synthetic read_file tool results — the agent sees the file
	// contents as if it had called read_file itself, without consuming a
	// turn. Paths are relative to the step's working directory and confined
	// to its workspace (resolveAgentPath); in practice each file lives
	// inside a declared input, e.g. ["repo/CLAUDE.md"]. Only valid on agent
	// steps. A missing, escaping, or over-100KB file fails the step at
	// preparation, before a token is spent.
	ContextPaths []string `yaml:"context_paths,omitempty"`
	// Hooks are the step's on_success/on_failure/on_error/on_abort/ensure
	// reaction steps (see Hooks). Inlined so they sit alongside the step's
	// own fields in YAML.
	Hooks Hooks `yaml:",inline"`
	// Assert, on a task/agent step, checks the step's captured output/exit
	// code (see Assert). A matching assert makes a non-zero-exit task a
	// success; a mismatch fails the step. Invalid on get/put steps.
	Assert *Assert `yaml:"assert,omitempty"`
	// Handoff, on an agent step, opts into transition context: when this step
	// is entered via a to:/verdicts: transition, it receives a machine-
	// assembled <transition_context> block (see HandoffSpec.Context) appended
	// to its prompt and/or a synthesized previous_run tool (see
	// HandoffSpec.Tool) it can call to pull the routed-from agent's recorded
	// run on demand. Agent-only, and only valid on a step that is the target
	// of at least one to: route within its own get-segment — see
	// validateHandoffSteps. On the step's first/unrouted execution, no
	// context block is appended and previous_run (if granted) answers "no
	// previous run" as data.
	Handoff *HandoffSpec `yaml:"handoff,omitempty"`
	// InputMapping/OutputMapping rename a task step's declared inputs/outputs
	// onto plan-artifact names, mirroring Concourse's input_mapping/
	// output_mapping (see docs/conformance.md;
	// TestRunJobIsolatedGetAliasMappingAndPutAll in
	// workspace_integration_test.go; Concourse doc: concourse-ci.org/docs/
	// steps/task/): each entry is {task-config-name: plan-artifact-name}, so
	// a reusable tasks: entry with pinned input names can be pointed at
	// whatever a job actually fetched/produced without editing the task. Keys
	// must be a subset of the resolved task's declared inputs:/outputs:. Task
	// steps only, and only meaningful under a workspace: block (mapping renames
	// a materialized directory, which the shared single directory can't do) —
	// both are load-time errors otherwise. Absent/empty leaves names unmapped.
	InputMapping  map[string]string `yaml:"input_mapping,omitempty"`
	OutputMapping map[string]string `yaml:"output_mapping,omitempty"`
	// Resource, on a get step, names the resource to fetch when it differs from
	// the step's own name: the fetched artifact (and the directory, step name,
	// and to: target) is Get, while the resource whose check/in runs is
	// Resource — mirroring Concourse's get.resource, including that two
	// aliased get steps for the same underlying resource share one version
	// history rather than tracking separately (see docs/conformance.md;
	// TestResourcesAndAffectedJobsResolveGetAlias in
	// internal/trigger/trigger_test.go; Concourse doc: concourse-ci.org/docs/
	// steps/get/). This lets one resource appear under a task-friendly name,
	// or twice in a plan under two names. Empty (the default) means the
	// resource name equals Get. Get steps only.
	Resource string `yaml:"resource,omitempty"`
}

// InputSpec is a step's inputs: declaration. It is a distinct type rather than
// a plain []string for two reasons: an absent inputs: key (nil *InputSpec)
// must be distinguishable from an explicit empty list (so ResolveTask can apply
// a step's inputs: over its tasks: entry only when the step actually declared
// one), and put steps accept the scalar `inputs: all` — every available
// artifact — in addition to a sequence of names. Names holds the explicit
// list; All is set only by the scalar form and is valid on put steps only.
type InputSpec struct {
	Names []string
	All   bool
}

// UnmarshalYAML decodes an InputSpec from either the scalar `all` or a
// sequence of artifact names. Any other scalar is rejected here so a typo like
// `inputs: repo` (meaning `inputs: [repo]`) fails loudly rather than silently
// parsing as a zero-name declaration.
func (in *InputSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias/mapping kinds that can't appear here
	case yaml.ScalarNode:
		var s string

		err := node.Decode(&s)
		if err != nil {
			return fmt.Errorf("inputs: %w", err)
		}

		if s != "all" {
			return fmt.Errorf("inputs: scalar value must be \"all\" (or use a sequence of names), got %q", s)
		}

		in.All = true

		return nil
	case yaml.SequenceNode:
		return node.Decode(&in.Names) //nolint:wrapcheck // yaml.v3 error is already descriptive
	default:
		return fmt.Errorf("inputs at line %d must be a sequence of names or the scalar \"all\"", node.Line)
	}
}

// Inputs constructs a declared *InputSpec from an explicit name list —
// Inputs() declares the empty "starts from nothing" form, Inputs("a", "b") a
// named list. It exists so callers building a Step programmatically (tests,
// and any future config synthesis) get the same declared-vs-absent distinction
// yaml.v3 gives a decoded pipeline.
func Inputs(names ...string) *InputSpec {
	if names == nil {
		names = []string{}
	}

	return &InputSpec{Names: names}
}

// InputsDeclared reports whether the step carried an inputs: key at all
// (nil vs present, including inputs: []). ResolveTask reads this so a step's
// inputs: override its tasks: entry only when the step actually declared one.
func (s Step) InputsDeclared() bool { return s.Inputs != nil }

// InputNames returns the explicit input names (nil for an absent or all-form
// inputs:). It is the []string every existing input consumer expects.
func (s Step) InputNames() []string {
	if s.Inputs == nil {
		return nil
	}

	return s.Inputs.Names
}

// InputsAll reports whether the step declared `inputs: all` (put steps only).
func (s Step) InputsAll() bool { return s.Inputs != nil && s.Inputs.All }

// GetResourceName is the name of the resource a get step fetches: Resource
// when set (get: aliases the resource under a different name), else Get itself.
// The fetched artifact, its directory, and the step's routing name are always
// Get; only the resource whose check/in runs is GetResourceName.
func (s Step) GetResourceName() string {
	if s.Resource != "" {
		return s.Resource
	}

	return s.Get
}

// HandoffSpec is a step's handoff: (see Step.Handoff). Context enables the
// pushed <transition_context> prompt block; Tool enables the pulled
// previous_run tool. A scalar `handoff: true` is shorthand for {context:
// true}; a mapping's context defaults to true unless explicitly set false,
// so `handoff: {tool: true}` enables both.
type HandoffSpec struct {
	Context bool
	Tool    bool
}

// UnmarshalYAML decodes a HandoffSpec from either a scalar (handoff: true/
// false, context only) or a mapping ({context, tool}) YAML node — the same
// scalar-or-mapping idiom as WhenSpec/FixSpec.
func (h *HandoffSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		var enabled bool

		err := value.Decode(&enabled)
		if err != nil {
			return fmt.Errorf("step handoff: %w", err)
		}

		h.Context = enabled

		return nil
	case yaml.MappingNode:
		var m struct {
			Context *bool `yaml:"context"`
			Tool    bool  `yaml:"tool"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step handoff: %w", err)
		}

		h.Context = m.Context == nil || *m.Context
		h.Tool = m.Tool

		return nil
	default:
		return fmt.Errorf("step handoff at line %d must be a boolean or a {context, tool} mapping", value.Line)
	}
}

// Enabled reports whether h turns on anything. A non-nil but all-false
// HandoffSpec (handoff: false, or handoff: {context: false}) is rejected at
// LoadConfig (validateHandoffSteps) rather than silently accepted as a no-op.
func (h *HandoffSpec) Enabled() bool {
	return h.Context || h.Tool
}

// FileRef is an agent step's prompt_file: — the text of the model's prompt
// for this step, loaded from a file rather than written inline. A scalar path
// is relative to the pipeline YAML's own directory and is resolved at load
// time, exactly like every other *_file field (see LoadConfig's
// resolveFileIncludes). A mapping {artifact, path} instead names a file
// inside an artifact this step declares in its inputs: — resolved at run
// time, once that artifact is fetched, since merkle.PlanChains hashes every
// step before any get's `in` has run (see internal/merkle's planGetStep) and
// so cannot see the file at plan time.
type FileRef struct {
	Path     string
	Artifact string
}

// UnmarshalYAML decodes a FileRef from either a scalar (a pipeline-relative
// path) or a mapping ({artifact, path}) YAML node — the same scalar-or-mapping
// idiom as WhenSpec/FixSpec/HandoffSpec.
func (f *FileRef) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&f.Path) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Artifact string `yaml:"artifact"`
			Path     string `yaml:"path"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("prompt_file: %w", err)
		}

		if m.Artifact == "" || m.Path == "" {
			return fmt.Errorf("prompt_file at line %d: an {artifact, path} mapping requires both fields", value.Line)
		}

		f.Artifact, f.Path = m.Artifact, m.Path

		return nil
	default:
		return fmt.Errorf("prompt_file at line %d must be a path or an {artifact, path} mapping", value.Line)
	}
}

// Deferred reports whether f names a run-time artifact file (the {artifact,
// path} mapping form) rather than a load-time, pipeline-relative one.
func (f *FileRef) Deferred() bool {
	return f != nil && f.Artifact != ""
}

// StepKind is which of Get/Task/Put/Agent a Step is. See Step.Kind.
type StepKind string

// The StepKind values, one per Step field Kind can resolve to.
const (
	StepKindGet   StepKind = "get"
	StepKindTask  StepKind = "task"
	StepKindPut   StepKind = "put"
	StepKindAgent StepKind = "agent"
)

// Kind reports which single kind of step s is. ok is false when zero, or
// more than one, of Get/Task/Put/Agent is set — a malformed step every call
// site should reject the same way, rather than each silently picking
// whichever field its own historical check order happened to test first.
func (s Step) Kind() (kind StepKind, ok bool) {
	for _, candidate := range [...]struct {
		kind StepKind
		set  bool
	}{
		{StepKindGet, s.Get != ""},
		{StepKindTask, s.Task != ""},
		{StepKindPut, s.Put != ""},
		{StepKindAgent, s.Agent != ""},
	} {
		if !candidate.set {
			continue
		}

		if ok {
			return "", false // a second kind field was set — reject, don't silently keep the first
		}

		kind, ok = candidate.kind, true
	}

	return kind, ok
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

	err = cfg.resolveFileIncludes(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	cfg.registerBuiltinAgents()

	err = cfg.resolveSubAgentDescriptions()
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

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
	checks := []func() error{
		c.validateWorkspace,
		c.validateArtifactDecls,
		c.validateGetResource,
		c.validateArtifactMappings,
		c.validateImageRules,
		c.validateTimeouts,
		c.validateAgentCompaction,
		c.validateHooks,
		c.validateAgentGraph,
		c.validateToolCallGuards,
		c.validateStepGuards,
		c.validateStepContextPaths,
		c.validateStepTransitions,
		c.validateAsserts,
		c.validateCredentialHandling,
	}

	for _, check := range checks {
		err := check()
		if err != nil {
			return err
		}
	}

	return nil
}

// validateCredentialHandling groups validateAgentEndpoints and every
// mcp_servers:-related check — split out of validate() itself to keep that
// function's branch count down (cyclop); all of it is trust-boundary
// validation around how a config references an external system's endpoint
// and credentials.
func (c *Config) validateCredentialHandling() error {
	err := c.validateAgentEndpoints()
	if err != nil {
		return err
	}

	err = c.validateMCPServers()
	if err != nil {
		return err
	}

	err = c.validateMCPToolGrants()
	if err != nil {
		return err
	}

	return c.validateResourceTypeConfig()
}

// reservedRouteKeys are the outcome keys with fixed meaning in a step's to:
// map: a verdict may not reuse one, and in binary (non-verdict) mode they are
// the only keys allowed. Keeping the set closed here is what reserves the rest
// of the key space for a future exit-code routing extension.
//
//nolint:gochecknoglobals // static, read-only lookup table
var reservedRouteKeys = map[string]bool{"success": true, "failure": true}

// stepName is the name a step is referenced by as a to: jump target: whichever
// of task/put/agent is set. Duplicated (not shared with internal/pipeline's
// executedStepName) because internal/config depends on nothing internal.
func stepName(step Step) string {
	kind, ok := step.Kind()
	if !ok {
		return ""
	}

	switch kind { //nolint:exhaustive // default covers StepKindGet, which is not a valid to: target
	case StepKindTask:
		return step.Task
	case StepKindAgent:
		return step.Agent
	case StepKindPut:
		return step.Put
	default:
		return ""
	}
}

// validateStepTransitions enforces the to:/max_visits:/verdicts: rules at load
// time: routing fields are invalid on get and hook steps; within any plan
// segment (bounded by get steps) that uses routing, step names must be unique,
// every to: target must resolve within the segment, a backward target requires
// max_visits, and an agent's verdict vocabulary must be complete and
// consistent with its to: keys. Also validates handoff: (validateHandoffSteps),
// since it's meaningless without a to: route targeting the step it's set on.
func (c *Config) validateStepTransitions() error {
	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitSteps(rejectRoutingOnGet)
		if err != nil {
			return err
		}

		err = job.visitHookSteps(rejectRoutingOnHook)
		if err != nil {
			return err
		}

		err = c.validatePlanSegments(job)
		if err != nil {
			return err
		}
	}

	return c.validateHandoffSteps()
}

// rejectRoutingOnGet rejects to:/max_visits:/verdicts: on a get step (a get
// fans the remainder of the plan out per version, so routing it is meaningless).
func rejectRoutingOnGet(label string, step *Step) error {
	if step.Get != "" && (step.To != nil || step.MaxVisits != 0 || len(step.Verdicts) > 0) {
		return fmt.Errorf("%s (get %q): to/max_visits/verdicts are not valid on get steps", label, step.Get)
	}

	return nil
}

// rejectRoutingOnHook rejects to:/verdicts: on a hook step (a hook is a
// reaction, not a positioned plan step, so it can't be a jump source or target).
func rejectRoutingOnHook(label string, step *Step) error {
	if step.To != nil || len(step.Verdicts) > 0 {
		return fmt.Errorf("%s: to/verdicts are not valid on hook steps", label)
	}

	return nil
}

// visitHookSteps calls fn for every hook step reachable from a job (each plan
// step's hooks, recursively, and each job-level hook) — but NOT the plan steps
// themselves, so a validator can treat hook steps differently from plan steps.
func (j Job) visitHookSteps(fn func(label string, step *Step) error) error {
	jobLabel := fmt.Sprintf("job %q", j.Name)

	for i := range j.Plan {
		err := j.Plan[i].Hooks.Each(func(name string, hook *Step) error {
			return visitStepTree(fmt.Sprintf("%s step %d (%s hook)", jobLabel, i, name), hook, fn)
		})
		if err != nil {
			return err
		}
	}

	return j.Hooks.Each(func(name string, hook *Step) error {
		return visitStepTree(fmt.Sprintf("%s %s hook", jobLabel, name), hook, fn)
	})
}

// validatePlanSegments splits a job's plan into segments at each get step and
// validates transitions within each segment that uses routing. A segment is a
// maximal run of consecutive non-get plan steps; a get is a boundary belonging
// to no segment. Segment-relative positions here match what internal/pipeline's
// runSteps sees, since runSteps re-enters over a truncated slice per get.
func (c *Config) validatePlanSegments(job Job) error {
	var segment []int // plan indices of the current segment's steps

	flush := func() error {
		if len(segment) == 0 {
			return nil
		}

		err := validateSegment(job, segment)
		segment = nil

		return err
	}

	for i := range job.Plan {
		if job.Plan[i].Get != "" {
			err := flush()
			if err != nil {
				return err
			}

			continue
		}

		segment = append(segment, i)
	}

	return flush()
}

// validateSegment validates the routing of one segment (plan indices in
// declaration order). It's a no-op unless some step in the segment uses
// routing; when it does, step names must be unique so they can be jump targets.
func validateSegment(job Job, segment []int) error {
	usesRouting := false

	for _, idx := range segment {
		if job.Plan[idx].To != nil || len(job.Plan[idx].Verdicts) > 0 {
			usesRouting = true

			break
		}
	}

	if !usesRouting {
		return nil
	}

	pos := make(map[string]int, len(segment))

	for segPos, idx := range segment {
		name := stepName(job.Plan[idx])

		_, dup := pos[name]
		if dup {
			return fmt.Errorf("job %q: step name %q is duplicated within a to:-using segment; names must be unique to be jump targets", job.Name, name)
		}

		pos[name] = segPos
	}

	for segPos, idx := range segment {
		err := validateStepRouting(job, idx, segPos, job.Plan[idx], pos)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateStepRouting validates one step's to:/verdicts:/max_visits: against
// its segment (pos maps each segment step name to its segment-relative
// position). segPos is this step's own position.
func validateStepRouting(job Job, planIdx, segPos int, step Step, pos map[string]int) error {
	label := fmt.Sprintf("job %q step %d", job.Name, planIdx)

	if len(step.Verdicts) > 0 {
		err := validateVerdictMode(label, step)
		if err != nil {
			return err
		}
	} else if step.To != nil {
		for key := range step.To {
			if !reservedRouteKeys[key] {
				return fmt.Errorf("%s: to: key %q is not valid (expected success or failure)", label, key)
			}
		}
	}

	return validateRouteTargets(label, segPos, step, pos)
}

// validateVerdictMode enforces the verdict-mode shape: agent-only, well-formed
// verdict names, and a complete, consistent mapping between the declared
// verdicts and the to: keys.
func validateVerdictMode(label string, step Step) error {
	if step.Agent == "" {
		return fmt.Errorf("%s: verdicts is only valid on agent steps", label)
	}

	if step.To == nil {
		return fmt.Errorf("%s: verdicts requires a to: map", label)
	}

	declared, err := validateVerdictNames(label, step)
	if err != nil {
		return err
	}

	return validateVerdictToKeys(label, step, declared)
}

// validateVerdictNames checks each declared verdict is non-empty, unique, not a
// reserved key, and has a to: target; it returns the set of declared names.
func validateVerdictNames(label string, step Step) (map[string]bool, error) {
	declared := make(map[string]bool, len(step.Verdicts))

	for _, verdict := range step.Verdicts {
		switch {
		case verdict == "":
			return nil, fmt.Errorf("%s: verdicts must not contain an empty name", label)
		case reservedRouteKeys[verdict]:
			return nil, fmt.Errorf("%s: verdict %q collides with a reserved key (success/failure)", label, verdict)
		case declared[verdict]:
			return nil, fmt.Errorf("%s: verdict %q is declared more than once", label, verdict)
		}

		declared[verdict] = true

		_, routed := step.To[verdict]
		if !routed {
			return nil, fmt.Errorf("%s: verdict %q has no to: target", label, verdict)
		}
	}

	return declared, nil
}

// validateVerdictToKeys checks every to: key in verdict mode is either the
// reserved failure catch or a declared verdict — and rejects a generic success
// key, since a verdict replaces it.
func validateVerdictToKeys(label string, step Step, declared map[string]bool) error {
	for key := range step.To {
		switch {
		case key == "success":
			return fmt.Errorf("%s: to: success is not valid in verdict mode (a verdict replaces generic success)", label)
		case key == "failure":
			continue // reserved catch for "never produced a verdict"
		case !declared[key]:
			return fmt.Errorf("%s: to: key %q is not a declared verdict", label, key)
		}
	}

	return nil
}

// validateRouteTargets resolves every to: target within the segment and
// requires max_visits when any target routes backward (segment-relative
// position at or before the declaring step).
func validateRouteTargets(label string, segPos int, step Step, pos map[string]int) error {
	backward := false

	for key, target := range step.To {
		targetPos, ok := pos[target]
		if !ok {
			return fmt.Errorf("%s: to: %s routes to %q, which is not a step in the same segment", label, key, target)
		}

		if targetPos <= segPos {
			backward = true
		}
	}

	if backward && step.MaxVisits <= 0 {
		return fmt.Errorf("%s: to: routes backward, so max_visits must be set (> 0)", label)
	}

	if backward && step.MaxVisits > maxVisitsLimit {
		return fmt.Errorf("%s: max_visits %d exceeds the maximum of %d", label, step.MaxVisits, maxVisitsLimit)
	}

	return nil
}

// validateHandoffSteps enforces the handoff: rules at load time: it is never
// valid on a hook step (a hook is a reaction, not a positioned step with
// predecessors), and on a plan step it is agent-only, must enable at least
// one of context/tool, and must be the target of at least one to: route
// within its own get-segment — otherwise the field is dead config.
func (c *Config) validateHandoffSteps() error {
	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitHookSteps(rejectHandoffOnHook)
		if err != nil {
			return err
		}

		err = c.validateHandoffSegments(job)
		if err != nil {
			return err
		}
	}

	return nil
}

// rejectHandoffOnHook rejects handoff: on a hook step.
func rejectHandoffOnHook(label string, step *Step) error {
	if step.Handoff != nil {
		return fmt.Errorf("%s: handoff is not valid on hook steps", label)
	}

	return nil
}

// validateHandoffSegments splits job's plan into get-bounded segments (the
// same split validatePlanSegments uses) and validates each segment's
// handoff: steps against it.
func (c *Config) validateHandoffSegments(job Job) error {
	var segment []int

	flush := func() error {
		if len(segment) == 0 {
			return nil
		}

		err := validateHandoffSegment(job, segment)
		segment = nil

		return err
	}

	for i := range job.Plan {
		if job.Plan[i].Get != "" {
			err := flush()
			if err != nil {
				return err
			}

			continue
		}

		segment = append(segment, i)
	}

	return flush()
}

// validateHandoffSegment checks every handoff: step in segment (plan indices
// in declaration order) is an agent step, enables something, and is named by
// at least one to: target anywhere in the segment.
func validateHandoffSegment(job Job, segment []int) error {
	targeted := map[string]bool{}

	for _, idx := range segment {
		for _, target := range job.Plan[idx].To {
			targeted[target] = true
		}
	}

	for _, idx := range segment {
		step := job.Plan[idx]
		if step.Handoff == nil {
			continue
		}

		label := fmt.Sprintf("job %q step %d", job.Name, idx)

		if step.Agent == "" {
			return fmt.Errorf("%s: handoff is only valid on agent steps", label)
		}

		if !step.Handoff.Enabled() {
			return fmt.Errorf("%s: handoff enables nothing (set context and/or tool)", label)
		}

		if !targeted[stepName(step)] {
			return fmt.Errorf("%s: handoff is set but no to: route in this segment targets step %q", label, stepName(step))
		}
	}

	return nil
}

// validateStepGuards rejects a when: guard on a get step (a get fans the
// remainder of the plan out per version, so gating one has no coherent
// meaning — the same reasoning that rejects image:/assert: there) and an
// empty guard command (which would otherwise run `sh -c ""`, exit 0, and
// silently mean "always run").
func (c *Config) validateStepGuards() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.When == nil {
				return nil
			}

			if step.Get != "" {
				return fmt.Errorf("%s (get %q): when is not valid on get steps", label, step.Get)
			}

			if strings.TrimSpace(step.When.Run) == "" {
				return fmt.Errorf("%s: when requires a command", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateStepContextPaths rejects context_paths: on non-agent steps — a
// path like "repo/CLAUDE.md" only has meaning when an agent step knows its
// workspace inputs, and the synthetic read_file injection at run time
// requires read_file to be in the tool grant.
func (c *Config) validateStepContextPaths() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.ContextPaths) == 0 {
				return nil
			}

			if step.Agent == "" {
				return fmt.Errorf("%s: context_paths is only valid on agent steps", label)
			}

			for _, p := range step.ContextPaths {
				if strings.TrimSpace(p) == "" {
					return fmt.Errorf("%s: context_paths must not contain an empty path", label)
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

// validateToolCallGuards rejects max_calls:/args: set on anything but a
// custom tool: both fields change what a custom tool's run: command executes
// with, and neither has meaning on a builtin (whose implementation is fixed
// Go code) or a sub-agent tool (whose only argument is the model-authored
// request string — pinning or budgeting that is not the shape this feature
// targets; revisit if a real need shows up).
func (c *Config) validateToolCallGuards() error {
	err := c.validateAgentToolCallGuards()
	if err != nil {
		return err
	}

	err = c.validateTaskFixToolCallGuards()
	if err != nil {
		return err
	}

	return c.validateStepToolCallGuards()
}

func (c *Config) validateAgentToolCallGuards() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkToolCallGuardSpecs(fmt.Sprintf("agent %q", agent.Name), agent.Tools)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateTaskFixToolCallGuards() error {
	for i := range c.Tasks {
		fix := c.Tasks[i].Fix
		if fix == nil {
			continue
		}

		err := checkToolCallGuardSpecs(fmt.Sprintf("task %q fix", c.Tasks[i].Name), fix.Tools)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateStepToolCallGuards() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			err := checkToolCallGuardSpecs(label, step.Tools)
			if err != nil {
				return err
			}

			if step.Fix == nil {
				return nil
			}

			return checkToolCallGuardSpecs(label+" fix", step.Fix.Tools)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkToolCallGuardSpecs runs validateToolCallGuardShape over every spec in
// specs, stopping at the first error.
func checkToolCallGuardSpecs(context string, specs []ToolSpec) error {
	for _, spec := range specs {
		err := validateToolCallGuardShape(context, spec)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateToolCallGuardShape rejects max_calls:/args: on a builtin or
// sub-agent tool, a negative max_calls: on any tool, and (since a mapping-
// form builtin — {builtin: name, ...} — is otherwise indistinguishable from a
// custom tool once other fields are present) a builtin mixed with name:/run:.
func validateToolCallGuardShape(context string, spec ToolSpec) error {
	if spec.Builtin != "" && (spec.Name != "" || spec.Run != "") {
		return fmt.Errorf("%s: builtin tool %q must not also set name/run", context, spec.Builtin)
	}

	if spec.MaxCalls < 0 {
		return fmt.Errorf("%s: tool %q: max_calls must be >= 0", context, ToolSpecName(spec))
	}

	err := validateMaxOutputBytesShape(context, spec)
	if err != nil {
		return err
	}

	guarded := spec.MaxCalls != 0 || spec.Args != nil
	if !guarded {
		return nil
	}

	if spec.Builtin != "" {
		return fmt.Errorf("%s: builtin tool %q: max_calls/args are only valid on custom tools", context, spec.Builtin)
	}

	if spec.Agent != "" {
		return fmt.Errorf("%s: sub-agent tool %q: max_calls/args are only valid on custom tools", context, spec.Agent)
	}

	return nil
}

// validateMaxOutputBytesShape enforces where max_output_bytes: may appear.
// It narrows a tool's inline output budget, which only makes sense for a
// tool whose output volume is not already a designed property: a built-in
// carries its own contract (read_file pages, list_dir counts, search_files
// is bounded by arithmetic), and a sub-agent's result is a considered
// answer rather than a data dump. See ToolSpec.MaxOutputBytes.
func validateMaxOutputBytesShape(context string, spec ToolSpec) error {
	if spec.MaxOutputBytes == 0 {
		return nil
	}

	if spec.MaxOutputBytes < 0 {
		return fmt.Errorf("%s: tool %q: max_output_bytes must be >= 0", context, ToolSpecName(spec))
	}

	if spec.Builtin != "" {
		return fmt.Errorf(
			"%s: builtin tool %q: max_output_bytes is only valid on custom tools and mcp grants; built-ins carry their own output contract",
			context, spec.Builtin,
		)
	}

	if spec.Agent != "" {
		return fmt.Errorf(
			"%s: sub-agent tool %q: max_output_bytes is only valid on custom tools and mcp grants",
			context, spec.Agent,
		)
	}

	return nil
}

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

// validateAsserts enforces which Assert fields are valid where: a Config- or
// Job-level assert may only set execution:; a task/agent step's assert may
// only set stdout:/code: (and code: only on tasks). A step assert is rejected
// on get/put steps. Hook steps are walked too (via visitSteps), so an assert
// on a hook task/agent gets the same treatment.
func (c *Config) validateAsserts() error {
	if c.Assert != nil {
		err := requireExecutionOnly("pipeline assert", c.Assert)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		if job.Assert != nil {
			err := requireExecutionOnly(fmt.Sprintf("job %q assert", job.Name), job.Assert)
			if err != nil {
				return err
			}
		}

		err := job.visitSteps(func(label string, step *Step) error {
			stepErr := validateStepAssert(label, step)
			if stepErr != nil {
				return stepErr
			}

			return c.validateAssertPinnedArgs(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAssertPinnedArgs rejects an assert.tool_calls entry that asserts on
// an argument the pipeline pins via a custom tool's args: (see ToolSpec.Args).
// A pinned value is machine-supplied and never appears among the
// model-authored arguments a trajectory records, so such an assert could never
// match — failing the load is far clearer than a step that always fails.
//
// Best-effort by design: it fires only when the agent resolves here. An
// unresolvable agent name is left to run time, matching how every other
// agent/task reference in this package is treated.
func (c *Config) validateAssertPinnedArgs(label string, step *Step) error {
	if step.Assert == nil || len(step.Assert.ToolCalls) == 0 || step.Agent == "" {
		return nil
	}

	agent, err := c.FindAgent(step.Agent)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable agent is caught at run time, same as everywhere else
	}

	pinned := pinnedArgsByTool(agent.Tools, step.Tools)

	for i, want := range step.Assert.ToolCalls {
		keys := make([]string, 0, len(want.Args))
		for key := range want.Args {
			keys = append(keys, key)
		}

		sort.Strings(keys) // deterministic message when several keys are pinned

		for _, key := range keys {
			if pinned[want.Name][key] {
				return fmt.Errorf("%s: assert.tool_calls[%d]: tool %q pins argument %q via args:, so it never appears in the model-authored call and can never match", label, i, want.Name, key)
			}
		}
	}

	return nil
}

// pinnedArgsByTool indexes which argument keys each named tool pins, across an
// agent's grant and a step's own inline tools.
func pinnedArgsByTool(agentTools, stepTools []ToolSpec) map[string]map[string]bool {
	index := map[string]map[string]bool{}

	add := func(specs []ToolSpec) {
		for _, spec := range specs {
			if len(spec.Args) == 0 {
				continue
			}

			name := ToolSpecName(spec)

			if index[name] == nil {
				index[name] = map[string]bool{}
			}

			for key := range spec.Args {
				index[name][key] = true
			}
		}
	}

	add(agentTools)
	add(stepTools)

	return index
}

// requireExecutionOnly rejects an execution-level assert (Config/Job) that
// carries the step-only stdout:/code: fields.
func requireExecutionOnly(label string, assert *Assert) error {
	if assert.Stdout != nil || assert.Code != nil {
		return fmt.Errorf("%s: stdout/code are only valid on task/agent step asserts, not an execution assert", label)
	}

	return nil
}

// validateStepAssert rejects a step assert that's misplaced (get/put) or
// carries the wrong fields for its step kind.
func validateStepAssert(label string, step *Step) error {
	if step.Assert == nil {
		return nil
	}

	if len(step.Assert.Execution) > 0 {
		return fmt.Errorf("%s: execution is only valid on job/pipeline asserts, not a step assert", label)
	}

	kind, ok := step.Kind()
	if !ok {
		return fmt.Errorf("%s: unrecognized step (must be get, task, put, or agent)", label)
	}

	switch kind { //nolint:exhaustive // default covers StepKindTask
	case StepKindGet:
		return fmt.Errorf("%s (get %q): assert is not valid on get steps", label, step.Get)
	case StepKindPut:
		return fmt.Errorf("%s (put %q): assert is not valid on put steps", label, step.Put)
	case StepKindAgent:
		if step.Assert.Code != nil {
			return fmt.Errorf("%s (agent %q): assert.code is not valid on agent steps (no exit code); use assert.stdout", label, step.Agent)
		}

		return validateExpectedToolCalls(fmt.Sprintf("%s (agent %q)", label, step.Agent), step.Assert.ToolCalls)
	default: // StepKindTask
		if len(step.Assert.ToolCalls) > 0 {
			return fmt.Errorf("%s: assert.tool_calls is only valid on agent steps (a task runs no tools)", label)
		}

		return nil
	}
}

// validateExpectedToolCalls rejects an assert.tool_calls entry with no name —
// there is nothing to match against, and an empty name would silently match
// the first call of any tool.
func validateExpectedToolCalls(context string, expected []ExpectedToolCall) error {
	for i, want := range expected {
		if want.Name == "" {
			return fmt.Errorf("%s: assert.tool_calls[%d]: name is required", context, i)
		}
	}

	return nil
}

// visitSteps calls fn for every step reachable from a job: each plan step,
// each job-level hook, and recursively every hook carried by any of those
// steps. label is a human-readable path such as
// `job "deploy" step 2 (on_failure hook)`, so a validator's error message
// points at the exact step. Used to give hook steps identical treatment to
// plan steps in the image/artifact/fix validators below.
func (j Job) visitSteps(fn func(label string, step *Step) error) error {
	jobLabel := fmt.Sprintf("job %q", j.Name)

	for i := range j.Plan {
		err := visitStepTree(fmt.Sprintf("%s step %d", jobLabel, i), &j.Plan[i], fn)
		if err != nil {
			return err
		}
	}

	return j.Hooks.Each(func(name string, step *Step) error {
		return visitStepTree(fmt.Sprintf("%s %s hook", jobLabel, name), step, fn)
	})
}

func visitStepTree(label string, step *Step, fn func(label string, step *Step) error) error {
	err := fn(label, step)
	if err != nil {
		return err
	}

	return step.Hooks.Each(func(name string, hook *Step) error {
		return visitStepTree(fmt.Sprintf("%s (%s hook)", label, name), hook, fn)
	})
}

// validateHooks enforces the hook-body restrictions: a hook must be a
// task/put/agent step (get is rejected — a get step fans the remainder of the
// plan out per version, which has no meaning inside a hook), and a job-level
// hook — and everything nested under it, recursively — may not declare
// inputs:/outputs: (a job-level hook runs in the job's own build workspace,
// which for a get-leading plan holds no artifacts; a nested hook runs in that
// exact same workspace, not a fresh one, so it has no more claim to a
// coherent artifact scope than its parent). Nested hooks recurse throughout.
func (c *Config) validateHooks() error {
	for _, job := range c.Jobs {
		for i := range job.Plan {
			err := validateHookTree(fmt.Sprintf("job %q step %d", job.Name, i), job.Plan[i].Hooks, false)
			if err != nil {
				return err
			}
		}

		err := validateHookTree(fmt.Sprintf("job %q", job.Name), job.Hooks, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// noArtifacts is true for a job-level hook and everything nested under it —
// see validateHooks.
func validateHookTree(parentLabel string, hooks Hooks, noArtifacts bool) error {
	return hooks.Each(func(name string, step *Step) error {
		label := fmt.Sprintf("%s (%s hook)", parentLabel, name)

		if noArtifacts && (step.InputsDeclared() || step.Outputs != nil) {
			return fmt.Errorf("%s: inputs/outputs are not valid on job-level hooks", label)
		}

		err := validateHookStep(label, step)
		if err != nil {
			return err
		}

		return validateHookTree(label, step.Hooks, noArtifacts)
	})
}

func validateHookStep(label string, step *Step) error {
	kind, ok := step.Kind()
	if !ok {
		return fmt.Errorf("%s: unrecognized hook step (must be task, put, or agent)", label)
	}

	if kind == StepKindGet {
		return fmt.Errorf("%s: get is not valid in a hook; hooks must be task, put, or agent steps", label)
	}

	return nil
}

// validateImageRules groups the three image:-related load-time checks
// (grouped into one call so config.validate's own branch count doesn't grow
// with every image: rule added — see cyclop): image: is invalid on get/put
// steps, an image: value must not look like a docker flag, and a fix: agent
// may not set its own image:.
func (c *Config) validateImageRules() error {
	err := c.validateImages()
	if err != nil {
		return err
	}

	err = c.validateImageValues()
	if err != nil {
		return err
	}

	return c.validateFixAgentImages()
}

// validateImages rejects image: on get/put steps: a put's execution image
// comes from its resource type (ResourceType.Image), and a get step has no
// task/agent to scope.
func (c *Config) validateImages() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Image == "" {
				return nil
			}

			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): image is not valid on get steps", label, step.Get)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): image is not valid on put steps; set it on the resource_type instead", label, step.Put)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateImageValues rejects an image: value that could be misread as a
// docker flag rather than an image reference: anything starting with '-'
// (e.g. "--privileged", "-v", "--network=host"). shell.dockerRunArgs also
// inserts a literal "--" before the image argument as defense in depth, but
// this check is what turns a mistyped or supply-chain-tainted image string
// into a clear LoadConfig error instead of docker silently granting whatever
// the flag means (privileged mode, an arbitrary bind mount, host
// networking). Checked wherever image: can be set: resource_types, agents,
// tasks, and steps (a step's own image: override).
func (c *Config) validateImageValues() error {
	for i := range c.ResourceTypes {
		rt := c.ResourceTypes[i]

		err := checkImageValue(fmt.Sprintf("resource_type %q", rt.Name), rt.Image)
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkImageValue(fmt.Sprintf("agent %q", agent.Name), agent.Image)
		if err != nil {
			return err
		}
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := checkImageValue(fmt.Sprintf("task %q", task.Name), task.Image)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return checkImageValue(label, step.Image)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkImageValue rejects an image value beginning with '-', which docker's
// argument parser would read as a flag rather than an image reference.
func checkImageValue(context, image string) error {
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("%s: image %q must not start with '-' (docker would parse it as a flag, not an image reference)", context, image)
	}

	return nil
}

// validateFixAgentImages rejects a fix: agent that sets its own image: —
// agent.RunFix always executes under the failing task's image (rt.Image),
// never the fix agent's own, so a fix agent's image: can never take effect.
// An unresolvable fix: agent name is left for FindAgent to catch at run
// time, same as everywhere else agent/task names aren't cross-checked at
// load time.
func (c *Config) validateFixAgentImages() error {
	check := func(context string, fix *FixSpec) error {
		if fix == nil {
			return nil
		}

		agent, err := c.FindAgent(fix.Agent)
		if err != nil {
			return nil //nolint:nilerr // unresolvable agent name is caught at run time, not here
		}

		if agent.Image != "" {
			return fmt.Errorf("%s: fix agent %q sets image: %q, but a fix loop always runs under the failing task's image, not the fix agent's own", context, fix.Agent, agent.Image)
		}

		return nil
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := check(fmt.Sprintf("task %q", task.Name), task.Fix)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return check(label, step.Fix)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateTimeouts checks that all timeout: fields contain valid Go duration
// strings (e.g., "2m", "30s", "1h30m"). Empty timeout is valid (no timeout).
func (c *Config) validateTimeouts() error {
	err := c.validateTaskTimeouts()
	if err != nil {
		return err
	}

	return c.validateStepTimeouts()
}

// validateAgentCompaction checks every agents: entry's compact_after_tokens:
// is not negative. Nil (unset) and 0 (explicitly disabled) are both valid;
// only a negative value has no meaning — mirrors ParseTimeout's own "reject
// negative, don't second-guess otherwise" precedent.
func (c *Config) validateAgentCompaction() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		if agent.CompactAfterTokens != nil && *agent.CompactAfterTokens < 0 {
			return fmt.Errorf("agent %q: compact_after_tokens must not be negative", agent.Name)
		}
	}

	return nil
}

// validateTaskTimeouts checks all tasks: entries for valid timeout values.
func (c *Config) validateTaskTimeouts() error {
	for _, task := range c.Tasks {
		if task.Timeout != "" {
			_, err := ParseTimeout(task.Timeout)
			if err != nil {
				return fmt.Errorf("task %q: %w", task.Name, err)
			}
		}

		if task.Fix != nil && task.Fix.Timeout != "" {
			_, err := ParseTimeout(task.Fix.Timeout)
			if err != nil {
				return fmt.Errorf("task %q fix: %w", task.Name, err)
			}
		}
	}

	return nil
}

// validateStepTimeouts checks all step timeout: fields for valid values.
func (c *Config) validateStepTimeouts() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Timeout != "" {
				_, err := ParseTimeout(step.Timeout)
				if err != nil {
					return fmt.Errorf("%s: %w", label, err)
				}
			}

			if step.Fix != nil && step.Fix.Timeout != "" {
				_, err := ParseTimeout(step.Fix.Timeout)
				if err != nil {
					return fmt.Errorf("%s fix: %w", label, err)
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
		found := false

		_ = job.visitSteps(func(_ string, step *Step) error {
			if step.Image != "" {
				found = true
			}

			return nil
		})

		if found {
			return true
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

func (c *Config) validateArtifactDecls() error {
	for i := range c.Tasks {
		task := c.Tasks[i]

		err := validateArtifactNames(fmt.Sprintf("task %q", task.Name), task.Inputs, task.Outputs)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return c.validateStepArtifactDecls(label, *step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateStepArtifactDecls(label string, step Step) error {
	err := c.validateMappingPlacement(label, step)
	if err != nil {
		return err
	}

	switch {
	case step.Get != "":
		if step.InputsDeclared() || step.Outputs != nil {
			return fmt.Errorf("%s (get %q): inputs/outputs are not valid on get steps", label, step.Get)
		}

		return nil
	case step.Put != "":
		if step.Outputs != nil {
			return fmt.Errorf("%s (put %q): outputs are not valid on put steps", label, step.Put)
		}

		// inputs: all is a put-only escape hatch; it names no artifacts, so
		// skip name validation for it.
		if step.InputsAll() {
			return nil
		}

		return validateArtifactNames(fmt.Sprintf("%s (put %q)", label, step.Put), step.InputNames(), nil)
	default:
		if step.InputsAll() {
			return fmt.Errorf("%s: inputs: all is only valid on put steps", label)
		}

		return validateArtifactNames(label, step.InputNames(), step.Outputs)
	}
}

// validateMappingPlacement enforces that input_mapping/output_mapping — which
// physically rename a materialized directory — appear only on task steps and
// only under a workspace: block.
func (c *Config) validateMappingPlacement(label string, step Step) error {
	if len(step.InputMapping) == 0 && len(step.OutputMapping) == 0 {
		return nil
	}

	if step.Task == "" {
		return fmt.Errorf("%s: input_mapping/output_mapping are only valid on task steps", label)
	}

	if c.Workspace == nil {
		return fmt.Errorf("%s: input_mapping/output_mapping require a top-level workspace: block", label)
	}

	return nil
}

// validateGetResource enforces that a step's resource: is only set on get
// steps and names an existing resource. The fetched resource is Resource when
// set, else Get (see Step.Resource); two get steps may alias the same resource
// under different names.
func (c *Config) validateGetResource() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Resource == "" {
				return nil
			}

			if step.Get == "" {
				return fmt.Errorf("%s: resource: is only valid on get steps", label)
			}

			_, err := c.FindResource(step.Resource)
			if err != nil {
				return fmt.Errorf("%s (get %q): %w", label, step.Get, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateArtifactMappings enforces that a task step's input_mapping/
// output_mapping keys are a subset of the resolved task's declared inputs/
// outputs — a mapping key that names no declared input/output is a typo that
// would otherwise silently do nothing. Placement rules (task-step-only,
// workspace-only) are enforced in validateStepArtifactDecls.
func (c *Config) validateArtifactMappings() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.InputMapping) == 0 && len(step.OutputMapping) == 0 {
				return nil
			}

			rt, err := c.ResolveTask(*step)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}

			err = checkMappingKeys(label, "input_mapping", step.InputMapping, rt.Inputs)
			if err != nil {
				return err
			}

			return checkMappingKeys(label, "output_mapping", step.OutputMapping, rt.Outputs)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkMappingKeys rejects a mapping key that names no declared artifact.
func checkMappingKeys(label, field string, mapping map[string]string, declared []string) error {
	if len(mapping) == 0 {
		return nil
	}

	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}

	for key := range mapping {
		if !declaredSet[key] {
			return fmt.Errorf("%s: %s key %q is not a declared %s", label, field, key,
				map[string]string{"input_mapping": "input", "output_mapping": "output"}[field])
		}
	}

	return nil
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

// FindMCPServer returns the mcp_servers: entry with the given name, or an
// error if not found.
func (c *Config) FindMCPServer(name string) (*MCPServer, error) {
	slog.Debug("mcp_server.find", "name", name)

	for i := range c.MCPServers {
		if c.MCPServers[i].Name == name {
			slog.Debug("mcp_server.find", "name", name, "found", true)

			return &c.MCPServers[i], nil
		}
	}

	return nil, fmt.Errorf("no mcp_servers entry named %q", name)
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
	// InputMapping/OutputMapping rename declared inputs/outputs onto plan-
	// artifact names under workspace: isolation (see Step.InputMapping). Always
	// from the step (tasks: entries carry no mapping); empty leaves names
	// unmapped.
	InputMapping  map[string]string
	OutputMapping map[string]string
	// Image, when non-empty, runs this task's run: (and any fix-loop
	// re-runs) in a container from this image instead of on the host. See
	// Task.Image/Step.Image.
	Image string
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// means no timeout. Step.Timeout overrides Task.Timeout when set.
	Timeout string
	// Assert, when set, checks the task's captured stdout/exit code (see
	// Assert). It always comes from the step (top-level tasks: entries carry
	// no assert), so a matching assert makes a non-zero-exit task a success.
	Assert *Assert
}

// ResolveTask resolves step into a ResolvedTask: a step carrying its own
// run: is inline and used as-is; otherwise step.Task names a tasks: entry,
// whose run/fix/inputs/outputs/image/timeout are used, except the step's own
// fix:, inputs:, outputs:, image:, and timeout:, if set (non-nil/non-empty),
// which override the referenced task's for this step only — the same override
// idiom for all five.
func (c *Config) ResolveTask(step Step) (ResolvedTask, error) {
	if step.Run != "" {
		return ResolvedTask{
			Name: step.Task, Run: step.Run, Fix: step.Fix,
			Inputs: step.InputNames(), Outputs: step.Outputs, Image: step.Image, Timeout: step.Timeout,
			InputMapping: step.InputMapping, OutputMapping: step.OutputMapping,
			Assert: step.Assert,
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
	if step.InputsDeclared() {
		inputs = step.InputNames()
	}

	outputs := task.Outputs
	if step.Outputs != nil {
		outputs = step.Outputs
	}

	image := task.Image
	if step.Image != "" {
		image = step.Image
	}

	timeout := task.Timeout
	if step.Timeout != "" {
		timeout = step.Timeout
	}

	return ResolvedTask{
		Name: step.Task, Run: task.Run, Fix: fix, Inputs: inputs, Outputs: outputs, Image: image, Timeout: timeout,
		InputMapping: step.InputMapping, OutputMapping: step.OutputMapping, Assert: step.Assert,
	}, nil
}

// defaultMaxAgentTurns is the default cap on one attempt's tool-calling loop
// when an agent doesn't set max_turns. 3-6 round trips covers a typical
// review (list a dir, read a few files, run a command, respond); 8 leaves
// headroom while still bounding a runaway loop (a model that never stops
// requesting tools) to a small, predictable number of calls.
const defaultMaxAgentTurns = 8

// defaultCompactAfterTokens is the conversation-size budget (in estimated
// tokens — see estimateContentTokens in internal/agent/compaction.go) an
// agent that sets no compact_after_tokens: gets: 80% of a 128K context
// window, the common size for current models.
//
// The 20% headroom is load-bearing, not padding. estimateContentTokens
// measures req.Contents alone — it never counts the system prompt, the tool
// schemas re-sent on every request, or the output the model still has to
// fit. A budget set at the full window would only ever fire after the
// request had already overflowed.
//
// This suits a 128K-or-larger model. A small local model (32K and under)
// overflows well before this and must set compact_after_tokens: lower;
// compact_after_tokens: 0 disables compaction entirely.
const defaultCompactAfterTokens = 128_000 * 80 / 100 // 102,400

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
	"opencode":   {"https://opencode.ai/zen/go/v1/", "OPENCODE_API_KEY", true},
	"anthropic":  {"https://api.anthropic.com/v1/", "ANTHROPIC_API_KEY", true},
}

// validateAgentEndpoints rejects an agents: entry whose source.endpoint:
// embeds userinfo (a "https://user:token@host/" style credential). Nothing
// in resolveAgentTarget/ResolveAgentInvocation scrubs it: the resolved
// BaseURL — endpoint included — is folded verbatim into AgentContentMap /
// subAgentInvocationContent (internal/merkle) and persisted through
// store.RecordNode, contradicting this codebase's own documented claim that
// hashed content excludes anything "secret-adjacent" (that exclusion only
// actually covers api_key_env's name/value, never a credential living in
// endpoint itself). Rejecting at load, rather than silently stripping the
// credential before hashing, surfaces the mistake immediately and points the
// operator at api_key_env — the mechanism this project already has for
// exactly this purpose — instead of a config that "works" while quietly
// leaking a credential into .steps/state.db on every run.
func (c *Config) validateAgentEndpoints() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		endpoint := agent.Source.Endpoint
		if endpoint == "" {
			continue
		}

		parsed, err := url.Parse(endpoint)
		if err != nil {
			continue // an unparsable endpoint is left for resolveAgentTarget/the HTTP client to reject at run time
		}

		if parsed.User != nil {
			return fmt.Errorf("agent %q: source.endpoint must not embed credentials (userinfo); use source.api_key_env instead", agent.Name)
		}
	}

	return nil
}

// validateMCPServers checks every mcp_servers: entry at load time: a
// non-empty, unique name; a transport shape consistent with exactly one of
// endpoint (http)/command (stdio) — see validateMCPServerTransport; for an
// http server, an endpoint that doesn't embed userinfo (same check and
// reasoning as validateAgentEndpoints — the endpoint is merkle-hashed, so it
// must not carry a credential); and an auth block shape consistent with its
// type ("" / "none" / "bearer" / "oauth" — "bearer" requires api_key_env,
// any other type must not set it).
func (c *Config) validateMCPServers() error {
	seen := make(map[string]bool, len(c.MCPServers))

	for i := range c.MCPServers {
		srv := c.MCPServers[i]

		if srv.Name == "" {
			return fmt.Errorf("mcp_servers[%d]: name is required", i)
		}

		if seen[srv.Name] {
			return fmt.Errorf("mcp_servers: name %q is declared more than once", srv.Name)
		}

		seen[srv.Name] = true

		err := validateMCPServerTransport(srv)
		if err != nil {
			return err
		}

		if srv.Endpoint != "" {
			parsed, parseErr := url.Parse(srv.Endpoint)
			if parseErr == nil && parsed.User != nil {
				return fmt.Errorf("mcp server %q: endpoint must not embed credentials (userinfo); use auth.api_key_env instead", srv.Name)
			}
		}

		err = validateMCPServerAuth(srv)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateMCPServerAuth checks one server's auth: block shape.
func validateMCPServerAuth(srv MCPServer) error {
	switch srv.Auth.Type {
	case "", "none":
		if srv.Auth.APIKeyEnv != "" {
			return fmt.Errorf("mcp server %q: api_key_env is only valid with auth.type: bearer", srv.Name)
		}
	case "bearer":
		if srv.Auth.APIKeyEnv == "" {
			return fmt.Errorf("mcp server %q: auth.type: bearer requires api_key_env", srv.Name)
		}
	case "oauth":
		if srv.Auth.APIKeyEnv != "" {
			return fmt.Errorf("mcp server %q: api_key_env is only valid with auth.type: bearer", srv.Name)
		}
	default:
		return fmt.Errorf("mcp server %q: auth.type must be one of none, bearer, oauth (got %q)", srv.Name, srv.Auth.Type)
	}

	return nil
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

// MCPToolNameSep separates the server and tool names in a single-tool MCP
// grant's reference name (ToolSpecName). Deliberately "__", not ".": the
// result is sent to the model as a function name, and OpenAI's (and most
// OpenAI-compatible providers') function-calling API restricts that to
// [a-zA-Z0-9_-] — a dot would be rejected outright.
const MCPToolNameSep = "__"

// ToolSpecName is the name a ToolSpec is referenced by: the builtin name for
// a builtin, the sub-agent's name for a sub-agent tool, "server__tool" (see
// MCPToolNameSep) for a single-tool MCP grant, the bare server name for a
// multi/all-tool MCP grant (selectable by a step as a unit — see
// resolveEffectiveTools), or the custom tool's name.
func ToolSpecName(spec ToolSpec) string {
	if spec.Builtin != "" {
		return spec.Builtin
	}

	if spec.Agent != "" {
		return spec.Agent
	}

	if spec.MCP != "" {
		if spec.MCPTool != "" {
			return spec.MCP + MCPToolNameSep + spec.MCPTool
		}

		return spec.MCP
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

		// A sub-agent is a capability grant, like run_shell: a step selects a
		// granted one by bare name (handled above), it may not introduce one
		// inline. validateAgentGraph rejects this at load; guard here too so
		// the invariant holds regardless of call path.
		if spec.Agent != "" {
			return nil, fmt.Errorf("sub-agent tool %q must be granted on the agent, not added inline on a step", spec.Agent)
		}

		// An MCP grant is a capability grant too, for the same reason —
		// validateMCPToolGrants rejects this at load; guard here too so the
		// invariant holds for the fix: path as well (RunFix funnels fix.Tools
		// through this same function, which has no separate load-time walk).
		if spec.MCP != "" {
			return nil, fmt.Errorf("mcp tool %q must be granted on the agent, not added inline on a step", spec.MCP)
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
	Description string
	BaseURL     string
	ModelName   string
	APIKeyEnv   string
	RequiresKey bool
	Persona     string
	// ContextPaths mirrors Step.ContextPaths once resolved — populated from
	// the step, not the agent definition, since concrete input paths are
	// only known at the step level.
	ContextPaths []string
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
	Timeout         string // wall-clock deadline per attempt; empty means no timeout
	// CompactAfterTokens is the already-resolved conversation-size budget (see
	// Agent.CompactAfterTokens); 0 means compaction is disabled for this
	// invocation.
	CompactAfterTokens int
	ToolSpecs          []ToolSpec
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
// the agent's config; agent.MaxTurns defaults to defaultMaxAgentTurns;
// agent.CompactAfterTokens, when nil, defaults to defaultCompactAfterTokens —
// unlike every other field resolved here, an explicit zero value is
// meaningfully different from "unset" and is preserved as 0, not defaulted).
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

	compactAfterTokens := defaultCompactAfterTokens
	if agent.CompactAfterTokens != nil {
		compactAfterTokens = *agent.CompactAfterTokens
	}

	image := agent.Image
	if step.Image != "" {
		image = step.Image
	}

	return ResolvedInvocation{
		AgentName:            agent.Name,
		Description:          agent.Description,
		BaseURL:              baseURL,
		ModelName:            modelName,
		APIKeyEnv:            apiKeyEnv,
		RequiresKey:          requiresKey,
		Persona:              agent.System,
		ContextPaths:         step.ContextPaths,
		Temperature:          agent.Temperature,
		TopP:                 agent.TopP,
		MaxTokens:            agent.MaxTokens,
		ReasoningEffort:      reasoning,
		MaxTurns:             maxTurns,
		Attempts:             attempts,
		Timeout:              step.Timeout,
		CompactAfterTokens:   compactAfterTokens,
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

// ParseTimeout parses a timeout string into a time.Duration. Empty string
// is valid and returns 0 (no timeout). Returns an error for invalid Go
// duration format (e.g., "2m", "30s", "1h30m" are valid; "2 minutes" is not).
func ParseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: %w", s, err)
	}

	if d < 0 {
		return 0, fmt.Errorf("invalid timeout %q: must not be negative", s)
	}

	return d, nil
}
