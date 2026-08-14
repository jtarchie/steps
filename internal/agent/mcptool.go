package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/shell"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildMCPTools resolves spec.MCP against cfg.MCPServers and connects once —
// eagerly, like buildSubAgentTool connects/validates its child agent eagerly
// — so a bad server config, missing credential, or unreachable server fails
// step preparation, not first call. It lists the server's tools and selects
// per spec's grant form (see config.ToolSpec.MCP/MCPTool/MCPTools): the one
// named tool, the named subset, or (both empty) every tool the server
// exposes. Returns one (declaration, toolImpl) pair per selected tool, all
// sharing the single connection, plus that connection as an io.Closer the
// caller (buildAgentTools) closes once the step ends — closing is not this
// function's job, since the returned toolImpls keep using the connection
// for the rest of the step.
func buildMCPTools(ctx context.Context, cfg *config.Config, spec config.ToolSpec) ([]*genai.FunctionDeclaration, map[string]toolImpl, io.Closer, error) {
	srv, err := cfg.FindMCPServer(spec.MCP)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp tool %q: %w", spec.MCP, err)
	}

	client, err := stepsmcp.Connect(ctx, *srv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp server %q: %w", spec.MCP, err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()

		return nil, nil, nil, fmt.Errorf("mcp server %q: %w", spec.MCP, err)
	}

	selected, err := selectMCPTools(spec, tools)
	if err != nil {
		_ = client.Close()

		return nil, nil, nil, err
	}

	decls := make([]*genai.FunctionDeclaration, 0, len(selected))
	registry := make(map[string]toolImpl, len(selected))

	for _, tool := range selected {
		name := spec.MCP + config.MCPToolNameSep + tool.Name

		err = checkProviderToolName(spec.MCP, tool.Name, name)
		if err != nil {
			_ = client.Close()

			return nil, nil, nil, err
		}

		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 name,
			Description:          mcpToolDescription(spec, tool),
			ParametersJsonSchema: tool.InputSchema,
		})

		registry[name] = mcpToolImpl(client, tool.Name, outputLimit(spec.MaxOutputBytes))
	}

	return decls, registry, client, nil
}

// maxProviderToolName is the function-name length OpenAI and most
// OpenAI-compatible providers accept.
const maxProviderToolName = 64

// providerToolName matches the character set those same providers allow in a
// function name — the constraint config.MCPToolNameSep's doc comment cites as
// the reason the separator is "__" rather than ".".
//
//nolint:gochecknoglobals // compiled once, read-only
var providerToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// checkProviderToolName rejects a joined server__tool name the provider will
// not accept, naming both halves.
//
// The server half is written by the pipeline author and validated at load.
// The remote half is not: it is whatever the MCP server advertises, and with
// a bare `{mcp: server}` grant the author never types those names at all. A
// server exposing `search.files`, or a name long enough to push the pair past
// the limit, therefore produced a 400 on every request for the whole step —
// from the provider, naming neither the MCP server nor the offending tool,
// about a string nothing in the pipeline mentions.
//
// Checked at step preparation rather than first call, matching how the rest
// of this file treats a bad grant.
func checkProviderToolName(server, remote, joined string) error {
	if !providerToolName.MatchString(joined) {
		return fmt.Errorf(
			"mcp server %q advertises tool %q, which cannot be offered to the model: %q is not a valid function name "+
				"(providers allow only letters, digits, underscore and hyphen). Grant specific tools with mcp_tools: to skip it",
			server, remote, joined)
	}

	if len(joined) > maxProviderToolName {
		return fmt.Errorf(
			"mcp server %q advertises tool %q, which cannot be offered to the model: %q is %d characters and the limit is %d. "+
				"Grant specific tools with mcp_tools: to skip it",
			server, remote, joined, len(joined), maxProviderToolName)
	}

	return nil
}

// mcpToolDescription returns spec.Description when the single-tool form
// overrides it, otherwise the server's own advertised description — a
// subset/all-tools grant always uses the server's description, since
// spec.Description is only meaningful (and only load-time-valid, per
// validateMCPToolShape) on the single-tool form.
func mcpToolDescription(spec config.ToolSpec, tool *sdkmcp.Tool) string {
	if spec.MCPTool != "" && spec.Description != "" {
		return spec.Description
	}

	return tool.Description
}

// selectMCPTools picks which of a server's advertised tools spec grants:
// the one named by MCPTool, the named subset in MCPTools, or (both empty)
// every tool. A named tool absent from the server's own list is a load-time-
// shaped error surfaced at step preparation, matching buildSubAgentTool's
// "fail preparation, not first call" behavior for a bad grant.
func selectMCPTools(spec config.ToolSpec, tools []*sdkmcp.Tool) ([]*sdkmcp.Tool, error) {
	byName := make(map[string]*sdkmcp.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}

	if spec.MCPTool != "" {
		tool, ok := byName[spec.MCPTool]
		if !ok {
			return nil, fmt.Errorf("mcp server %q has no tool named %q", spec.MCP, spec.MCPTool)
		}

		return []*sdkmcp.Tool{tool}, nil
	}

	if len(spec.MCPTools) > 0 {
		selected := make([]*sdkmcp.Tool, 0, len(spec.MCPTools))

		for _, name := range spec.MCPTools {
			tool, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("mcp server %q has no tool named %q", spec.MCP, name)
			}

			selected = append(selected, tool)
		}

		return selected, nil
	}

	return tools, nil
}

// mcpToolImpl returns a toolImpl that calls name on client and translates
// the result to the map shape every other toolImpl returns — "failure is
// data, not a Go error" (see toolImpl's doc comment): a transport/connect
// error becomes {"error": ...}; a successful CallToolResult with IsError
// true becomes {"error": <joined text content>}, still not a Go error, so
// required:/max_calls: enforcement (keyed on this returned map, not a
// returned error — see runAgentConversation) treats it exactly like a
// custom tool's nonzero exit. A successful result becomes
// {"structured_content": ..., "content": ...} — both keys always present
// (nil/empty when absent) so the shape stays predictable for the model and
// for assert.tool_calls matching.
func mcpToolImpl(client stepsmcp.Client, name string, limit int) toolImpl {
	return func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
		result, err := client.CallTool(ctx, name, args)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		text := joinTextContent(result.Content)

		if result.IsError {
			if text == "" {
				text = fmt.Sprintf("mcp tool %q returned an error with no text content", name)
			}

			return map[string]any{"error": spillOrTruncateLimit(text, limit, env.spillDir)}
		}

		return map[string]any{
			"structured_content": boundedStructuredContent(result.StructuredContent, limit, env.spillDir),
			"content":            spillOrTruncateLimit(text, limit, env.spillDir),
		}
	}
}

// boundedStructuredContent caps a tool result's structured content at limit
// — the grant's resolved inline budget (maxToolOutputBytes unless the grant
// tuned it via max_output_bytes:), the same bound spillOrTruncateLimit
// enforces on text —
// without this, a large structured payload would flood the model's context
// window unbounded, bypassing the cap every other tool's output already
// honors (and the SDK often mirrors the same payload into text content too,
// so an uncapped structured_content is frequently a second, unbounded copy
// of data content already carries a capped copy of). nil input (the common
// "this tool has no structured output" case) passes through as nil. An
// oversized payload is spilled to a file — the same as oversized text content
// — and the model gets a pointer to it.
//
// What it deliberately never does is hand back a BYTE PREFIX of the marshaled
// JSON. spillOrTruncate degrades to truncateToolOutput when spilling isn't
// possible (spillDir unset, or a create/write/close failure), which is the
// right answer for prose but produces syntactically invalid JSON here — a
// result that looks complete, parses as nothing, and misleads the model about
// what the tool returned. So this branches on spillToFile's ok directly and
// falls back to prose that says what happened, pointing at the text content,
// which the SDK usually populates with the same payload anyway.
func boundedStructuredContent(sc any, limit int, spillDir string) any {
	if sc == nil {
		return nil
	}

	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Sprintf("[structured content omitted: could not marshal: %s]", err.Error())
	}

	if len(data) <= limit {
		return sc
	}

	path, ok := spillToFile(string(data), spillDir)
	if !ok {
		return fmt.Sprintf(
			"[structured content omitted: %s of JSON exceeded the %s inline limit and could not be saved to a file;"+
				" this tool's text content carries the same payload]",
			shell.FormatBytes(len(data)), shell.FormatBytes(limit),
		)
	}

	return shell.SpillPointerMessage(len(data), path, spillPreview(string(data)))
}

// joinTextContent concatenates every TextContent block in content,
// newline-separated. Non-text content (images, audio, embedded resources)
// is out of scope for v1 — an agent tool result is fed back to the model as
// text, and this codebase has no multi-modal tool-result path today.
func joinTextContent(content []sdkmcp.Content) string {
	texts := make([]string, 0, len(content))

	for _, c := range content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}

	return strings.Join(texts, "\n")
}
