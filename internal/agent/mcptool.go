package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
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

		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 name,
			Description:          mcpToolDescription(spec, tool),
			ParametersJsonSchema: tool.InputSchema,
		})

		registry[name] = mcpToolImpl(client, tool.Name)
	}

	return decls, registry, client, nil
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
func mcpToolImpl(client stepsmcp.Client, name string) toolImpl {
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

			return map[string]any{"error": spillOrTruncate(text, env.spillDir)}
		}

		return map[string]any{
			"structured_content": boundedStructuredContent(result.StructuredContent, env.spillDir),
			"content":            spillOrTruncate(text, env.spillDir),
		}
	}
}

// boundedStructuredContent caps a tool result's structured content at
// maxToolOutputBytes, the same bound spillOrTruncate enforces on text —
// without this, a large structured payload would flood the model's context
// window unbounded, bypassing the cap every other tool's output already
// honors (and the SDK often mirrors the same payload into text content too,
// so an uncapped structured_content is frequently a second, unbounded copy
// of data content already carries a capped copy of). nil input (the common
// "this tool has no structured output" case) passes through as nil. An
// oversized payload is spilled to a file — via spillOrTruncate on its
// marshaled JSON — the same as oversized text content, rather than being
// replaced outright with an "omitted" message.
func boundedStructuredContent(sc any, spillDir string) any {
	if sc == nil {
		return nil
	}

	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Sprintf("[structured content omitted: could not marshal: %s]", err.Error())
	}

	if len(data) <= maxToolOutputBytes {
		return sc
	}

	return spillOrTruncate(string(data), spillDir)
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
