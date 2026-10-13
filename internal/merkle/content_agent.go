package merkle

import (
	"fmt"
	"slices"

	"github.com/jtarchie/steps/internal/config"
)

// toolSpecsContent builds the hashed content for an agent's effective tool
// list. A builtin/custom tool hashes exactly as before this helper existed
// ({builtin, name, description, run}) — so an agent with no sub-agent tools
// hashes byte-identically. A sub-agent tool (see config.ToolSpec.Agent) folds
// in the child agent's own resolved invocation content
// (subAgentInvocationContent), recursively, so editing a child — or a
// grandchild — busts the parent step's hash. Recursion terminates because
// LoadConfig's validateAgentGraph rejects cycles and caps nesting depth.
func toolSpecsContent(cfg *config.Config, specs []config.ToolSpec) ([]map[string]any, error) {
	out := make([]map[string]any, len(specs))

	for i, t := range specs {
		if t.Agent != "" {
			invocation, err := subAgentInvocationContent(cfg, t.Agent)
			if err != nil {
				return nil, err
			}

			out[i] = map[string]any{
				"agent":       t.Agent,
				"description": t.Description,
				"invocation":  invocation,
			}

			continue
		}

		if t.MCP != "" {
			content, err := mcpToolSpecContent(cfg, t)
			if err != nil {
				return nil, err
			}

			out[i] = content

			continue
		}

		content := map[string]any{
			"builtin":     t.Builtin,
			"name":        t.Name,
			"description": t.Description,
			"run":         t.Run,
		}

		// max_calls/args fold in only when set — a tool with neither hashes
		// byte-identically to before this feature existed. Both change what a
		// call actually executes with (a budget bounds it; pinned args alter
		// its arguments), the same argument TaskNodeContent's doc comment
		// makes for image: over the workspace-gated inputs/outputs.
		if t.MaxCalls != 0 {
			content["max_calls"] = t.MaxCalls
		}

		if len(t.Args) != 0 {
			content["args"] = t.Args
		}

		// Same value-gating: max_output_bytes narrows what a call returns to
		// the model, so it changes the conversation the step produces.
		if t.MaxOutputBytes != 0 {
			content["max_output_bytes"] = t.MaxOutputBytes
		}

		withRequiredContent(t, content)

		// web_fetch's host allow-list decides what the agent can reach at all, so narrowing it must re-run the step that was cached while it was wide. Sorted, because a host list is a set: reordering it changes nothing a fetch can see. Value-gated, and refused at load on every other tool form.
		if len(t.Allow) != 0 {
			content["allow"] = config.StableStrings(t.Allow)
		}

		withAskUserContent(t, content)

		out[i] = content
	}

	return out, nil
}

// withRequiredContent folds required: in. It is a success criterion — a required tool's nonzero exit aborts the step instead of being handed to the model as data — and assertContent's argument applies word for word: a step cached BEFORE the line was added would be skipped, and the criterion never evaluated on the run that added it. Value-gated like everything here.
func withRequiredContent(t config.ToolSpec, content map[string]any) {
	if t.Required {
		content["required"] = true
	}
}

// mcpToolSpecContent builds the hashed content for one of the three MCP
// tool-grant forms (see config.ToolSpec.MCP/MCPTool/MCPTools): mcp_tool for
// the single-tool form, mcp_tools (sorted, for hash determinism regardless
// of declaration order) for the named-subset form, or neither for the bare
// "grant everything" form — which is a deliberate, documented limitation:
// this package depends on config/resource only (never internal/mcp), so it
// cannot list a live server's tools at plan time, and the bare form's hash
// is therefore a static marker that a server's own tool list changing does
// not, by itself, bust. description/max_calls/args fold in only when set, the
// same value-gating every other tool kind here uses — args because a pin is
// the boundary the grant draws, so moving it must re-run the step.
func mcpToolSpecContent(cfg *config.Config, t config.ToolSpec) (map[string]any, error) {
	server, err := mcpServerContent(cfg, t.MCP)
	if err != nil {
		return nil, err
	}

	content := map[string]any{"mcp": t.MCP, "server": server}

	if t.MCPTool != "" {
		content["mcp_tool"] = t.MCPTool
	}

	withRequiredContent(t, content)

	if len(t.MCPTools) != 0 {
		sorted := slices.Clone(t.MCPTools)
		slices.Sort(sorted)
		content["mcp_tools"] = sorted
	}

	if t.Description != "" {
		content["description"] = t.Description
	}

	if t.MaxCalls != 0 {
		content["max_calls"] = t.MaxCalls
	}

	if len(t.Args) != 0 {
		content["args"] = t.Args
	}

	if t.MaxOutputBytes != 0 {
		content["max_output_bytes"] = t.MaxOutputBytes
	}

	return content, nil
}

// mcpServerContent builds the hashed identity of a configured mcp_servers:
// entry, folded into any tool grant or resource-type stage that references
// it. Endpoint/auth type/the api_key_env *name* (never its value, mirroring
// AgentSource's api_key_env exclusion exactly)/scopes determine behavior
// and are hashed; nothing token-shaped is ever computed here, since this
// package never imports internal/mcp. For a stdio server, command/args/cwd
// are its equivalent spawn identity and are value-gated the same way —
// note args is NOT sorted (unlike mcp_tools below): argv order is
// semantic, so reordering it must bust the hash. endpoint/auth_type stay
// unconditional so an existing HTTP server's hash is unaffected by this
// field set existing.
func mcpServerContent(cfg *config.Config, name string) (map[string]any, error) {
	srv, err := cfg.FindMCPServer(name)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", name, err)
	}

	content := map[string]any{
		"name":      srv.Name,
		"endpoint":  srv.Endpoint,
		"auth_type": srv.Auth.Type,
	}

	if srv.Auth.APIKeyEnv != "" {
		content["api_key_env"] = srv.Auth.APIKeyEnv
	}

	// A client_id is a public application identifier, not a credential, so
	// unlike the secret beside it the VALUE is hashed: pointing a server at
	// a different registered app changes who the calls are made as.
	if srv.Auth.ClientID != "" {
		content["client_id"] = srv.Auth.ClientID
	}

	if srv.Auth.ClientSecretEnv != "" {
		content["client_secret_env"] = srv.Auth.ClientSecretEnv
	}

	if len(srv.Auth.Scopes) != 0 {
		content["scopes"] = srv.Auth.Scopes
	}

	if srv.Command != "" {
		content["command"] = srv.Command
	}

	if len(srv.Args) != 0 {
		content["args"] = srv.Args
	}

	if srv.Cwd != "" {
		content["cwd"] = srv.Cwd
	}

	return content, nil
}

// subAgentInvocationContent builds the hashed identity of a sub-agent as
// invoked through a tool: everything that determines its output regardless of
// the per-call request — resolved model/endpoint, persona, dials, max_turns,
// image, and its own tool set (recursively). Prompt/dir/inputs/outputs/assert/
// hooks are deliberately excluded: a sub-agent has no step, so those are not
// part of its identity. The API key and its env var name are excluded for the
// same reason AgentContentMap excludes them.
func subAgentInvocationContent(cfg *config.Config, name string) (map[string]any, error) {
	ri, err := cfg.ResolveAgentInvocation(config.Step{Agent: name})
	if err != nil {
		return nil, fmt.Errorf("sub-agent %q: %w", name, err)
	}

	tools, err := toolSpecsContent(cfg, ri.ToolSpecs)
	if err != nil {
		return nil, err
	}

	content := map[string]any{
		"agent":            name,
		"model":            ri.ModelName,
		"endpoint":         ri.BaseURL,
		"system":           ri.Persona,
		"temperature":      ri.Temperature,
		"top_p":            ri.TopP,
		"max_tokens":       ri.MaxTokens,
		"reasoning_effort": ri.ReasoningEffort,
		"max_turns":        ri.MaxTurns,
		"tools":            tools,
	}

	if ri.Image != "" {
		content["image"] = ri.Image
	}

	// Names only — see the get node's env comment.
	if len(ri.Env) > 0 {
		content["env"] = sortedEnv(ri.Env)
	}

	if ri.User != "" {
		content["user"] = ri.User
	}

	if ri.Network != "" {
		content["network"] = ri.Network
	}

	withIsolation(ri.Privileged, ri.Limits, content)

	return content, nil
}

// withAskUserContent folds ask_user's own dials into a builtin grant's
// content, value-gated the way max_calls: and args: are. Each one changes what
// comes back into the conversation: answered_by: decides who answers at all,
// default: decides what the model is told when nobody does, and
// options_required: decides whether an answer is accepted at all.
func withAskUserContent(spec config.ToolSpec, content map[string]any) {
	if spec.AnsweredBy != "" {
		content["answered_by"] = spec.AnsweredBy
	}

	if spec.Default != "" {
		content["default"] = spec.Default
	}

	if spec.OptionsRequired {
		content["options_required"] = spec.OptionsRequired
	}
}

// withMemoryContent keys the file naming the scope, not the scope or what it
// holds: both are read when the step runs, and internal/agent folds a digest
// of them in under memory_preload before the node is hashed.
func withMemoryContent(step config.Step, content map[string]any) {
	if step.Memory != nil {
		content["memory"] = step.Memory.ScopeFrom
	}
}

// AgentContentMap is the content hashed for an agent node: everything that
// determines the model's output (agent, prompt, dir, resolved model/endpoint,
// persona, dials, and the effective tool set — including any sub-agent tools,
// folded in via toolSpecsContent). Attempts is excluded (a pure retry policy
// doesn't change the intended result); CompactAfterTokens is excluded for the
// same reason — it's an operational context-budget knob governing how a
// conversation manages its own history, not a determinant of the intended
// result, so a pipeline gains or loses compaction without invalidating any
// cached step; the API key and its env var name are excluded (nothing
// secret-adjacent belongs in hashed content). inputs/outputs are folded in
// unconditionally — see TaskNodeContent's doc comment for why.
func AgentContentMap(cfg *config.Config, step config.Step, ri config.ResolvedInvocation) (map[string]any, error) {
	toolsContent, err := toolSpecsContent(cfg, ri.ToolSpecs)
	if err != nil {
		return nil, err
	}

	content := map[string]any{
		"agent":            step.Agent,
		"messages":         config.StableStrings(step.Messages),
		"dir":              step.Dir,
		"model":            ri.ModelName,
		"endpoint":         ri.BaseURL,
		"system":           ri.Persona,
		"temperature":      ri.Temperature,
		"top_p":            ri.TopP,
		"max_tokens":       ri.MaxTokens,
		"reasoning_effort": ri.ReasoningEffort,
		"max_turns":        ri.MaxTurns,
		"tools":            toolsContent,
	}

	// Value-gated on the grant, unlike max_turns: beside it. A budget for
	// questions a step cannot ask changes nothing about what it does, and
	// folding it in unconditionally would invalidate every cached agent step
	// in every pipeline to record a dial with nothing to bound.
	if config.GrantsAskUser(ri.ToolSpecs) {
		content["max_questions"] = ri.MaxQuestions
	}

	content["inputs"] = config.StableStrings(step.InputNames())
	content["outputs"] = config.StableStrings(step.Outputs)

	withMemoryContent(step, content)

	if len(ri.ContextPaths) > 0 {
		// Paths, not file contents: the files live inside the step's
		// workspace (loadContextBlocks confines them), so their content is
		// already chained through the input artifacts' own hashes.
		content["context_paths"] = config.StableStrings(ri.ContextPaths)
	}

	if ri.Image != "" {
		content["image"] = ri.Image
	}

	// Names only — see the get node's env comment.
	if len(ri.Env) > 0 {
		content["env"] = sortedEnv(ri.Env)
	}

	if ri.User != "" {
		content["user"] = ri.User
	}

	if ri.Network != "" {
		content["network"] = ri.Network
	}

	withIsolation(ri.Privileged, ri.Limits, content)

	// Which CLI runs the conversation, when one does — value-gated so every
	// pre-existing HTTP agent hashes exactly as it did before CLI sources
	// existed. The CLI's own version is deliberately NOT folded in: it changes
	// under the operator the same way a hosted model's weights do, and this
	// package has never claimed to hash the thing on the other end of the
	// wire, only which thing was asked.
	if ri.CLI != "" {
		content["cli"] = ri.CLI
		// Which CLI setting scopes load — a project .claude/ shapes the
		// conversation, so opting in (or out) must invalidate the cache.
		content["cli_settings"] = ri.CLISettings
	}

	if step.Assert != nil {
		content["assert"] = assertContent(step.Assert)
	}

	return withStepModifiers(cfg, step, content)
}
