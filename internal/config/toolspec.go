package config

// A tools: entry in all its forms (builtin, custom, sub-agent, MCP grant):
// how it decodes, what it is named, and how a step's selection resolves
// against its agent's grant.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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
