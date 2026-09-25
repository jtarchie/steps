package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
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

		// ponytail: checked against the schema listed at preparation; a server
		// that changes its tool list mid-step is not re-checked until the next
		// step. Re-validate on the list-changed notification if one ever does.
		schema, pins, err := pinMCPArgs(spec, tool)
		if err != nil {
			_ = client.Close()

			return nil, nil, nil, err
		}

		decls = append(decls, &genai.FunctionDeclaration{
			Name:                 name,
			Description:          mcpToolDescription(spec, tool),
			ParametersJsonSchema: schema,
		})

		registry[name] = mcpToolImpl(client, tool.Name, outputLimit(spec.MaxOutputBytes), pins)
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
//
// pins, when non-nil, are merged over the model's arguments before the call
// goes over the wire (see mcpPins.apply).
func mcpToolImpl(client stepsmcp.Client, name string, limit int, pins *mcpPins) toolImpl {
	return func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
		if pins != nil {
			merged, overridden := pins.apply(args)
			if len(overridden) > 0 {
				// Key names only: the model's value may be injected text, and
				// this note is persisted and drawn in the web transcript.
				// ponytail: custom tools override silently; share this note
				// with execCustomTool when someone asks.
				events.Note(ctx, events.NoteWarn, fmt.Sprintf(
					"%s: model supplied pinned argument(s) %s; pinned values used", pins.tool, quoteJoin(overridden)))
			}

			args = merged
		}

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

// mcpPins is what pinMCPArgs resolved for one pinned grant: the typed values
// to send, and every property the tool declares, which is what tells a
// case-variant of a pinned key apart from a real parameter.
type mcpPins struct {
	tool     string
	values   map[string]any
	declared map[string]bool
}

// apply merges the pins over args and reports, sorted, every model-supplied
// key it overrode or dropped.
//
// A key that case-folds to a pinned one and is NOT a property the tool
// declares under that exact spelling is dropped: Go's encoding/json matches
// struct fields case-insensitively, so `Project_ID: 999` next to the pinned
// `project_id: 307` would let key order decide what a Go server binds.
func (p *mcpPins) apply(args map[string]any) (map[string]any, []string) {
	kept := make(map[string]any, len(args))

	var overridden []string

	for key, value := range args {
		if p.shadows(key) {
			overridden = append(overridden, key)

			continue
		}

		kept[key] = value
	}

	sort.Strings(overridden)

	return mergePinnedArgs(kept, p.values), overridden
}

// shadows reports whether a model-supplied key would compete with a pin.
func (p *mcpPins) shadows(key string) bool {
	if _, pinned := p.values[key]; pinned {
		return true
	}

	if p.declared[key] {
		return false
	}

	for pinned := range p.values {
		if strings.EqualFold(key, pinned) {
			return true
		}
	}

	return false
}

// pinMCPArgs checks spec's args: pins against tool's advertised input schema
// and returns the schema the model is shown — a copy with every pinned key
// removed from properties and required — plus the pins to merge at call
// time. With no pins it returns the schema untouched and nil pins.
//
// A pin that cannot bind is refused rather than sent: one the schema does not
// declare, or a value that does not convert to the declared type. Messages
// name the key and type, never the value — they travel over HTTP, reach the
// web UI and are persisted.
//
// ponytail: only top-level properties are understood. $ref/allOf schemas and
// nested paths are refused rather than resolved; resolve them when a real
// server needs it. An alias the server accepts under another name
// (project_id and projectId) cannot be seen from here at all.
func pinMCPArgs(spec config.ToolSpec, tool *sdkmcp.Tool) (any, *mcpPins, error) {
	if len(spec.Args) == 0 {
		return tool.InputSchema, nil, nil
	}

	name := spec.MCP + config.MCPToolNameSep + tool.Name

	schema, props := copySchema(tool.InputSchema)
	if schema == nil || len(props) == 0 {
		return nil, nil, fmt.Errorf("mcp tool %q: args: pins %s, but the server's input schema declares no properties to pin",
			name, quoteJoin(sortedKeys(spec.Args)))
	}

	pins := &mcpPins{tool: name, values: make(map[string]any, len(spec.Args)), declared: make(map[string]bool, len(props))}
	for key := range props {
		pins.declared[key] = true
	}

	for _, key := range sortedKeys(spec.Args) {
		prop, ok := props[key]
		if !ok {
			return nil, nil, fmt.Errorf("mcp tool %q: args: pins %q, which the server does not declare (it declares: %s). Run: steps mcp tools <pipeline> %s",
				name, key, strings.Join(sortedKeys(pins.declared), ", "), spec.MCP)
		}

		kind := pinType(prop)

		value, ok := convertPin(kind, spec.Args[key])
		if !ok {
			return nil, nil, fmt.Errorf("mcp tool %q: args: pins %q as %s, but the pinned value is not %s %s",
				name, key, kind, article(kind), kind)
		}

		pins.values[key] = value

		delete(props, key)
	}

	if required, ok := schema["required"].([]any); ok {
		schema["required"] = slices.DeleteFunc(required, func(entry any) bool {
			key, isString := entry.(string)
			_, pinned := pins.values[key]

			return isString && pinned
		})
	}

	return schema, pins, nil
}

// copySchema deep-copies an input schema by JSON round trip — whatever
// concrete type the SDK decoded into — so the original is never mutated, and
// returns the copy's properties map. An unreadable schema has no properties.
func copySchema(src any) (map[string]any, map[string]any) {
	var schema map[string]any

	data, err := json.Marshal(src)
	if err != nil || json.Unmarshal(data, &schema) != nil {
		return nil, nil
	}

	props, _ := schema["properties"].(map[string]any)

	return schema, props
}

// pinType resolves a property's scalar JSON type: a single type, the one
// non-null entry of a type array, or the one non-null branch of anyOf/oneOf
// (pydantic's Optional[int], a nullable zod schema). Anything else is "" and
// the pin is sent as a string.
func pinType(prop any) string {
	schema, _ := prop.(map[string]any)

	switch kind := schema["type"].(type) {
	case string:
		return kind
	case []any:
		var nonNull []any

		for _, entry := range kind {
			if entry != "null" {
				nonNull = append(nonNull, entry)
			}
		}

		if len(nonNull) == 1 {
			single, _ := nonNull[0].(string)

			return single
		}

		return ""
	}

	for _, combinator := range []string{"anyOf", "oneOf"} {
		branches, _ := schema[combinator].([]any)

		var nonNull []any

		for _, branch := range branches {
			if pinType(branch) != "null" {
				nonNull = append(nonNull, branch)
			}
		}

		if len(nonNull) == 1 {
			return pinType(nonNull[0])
		}
	}

	return ""
}

// convertPin turns a pinned string into kind. ok is false when it does not
// parse, or when kind is one no string can satisfy; any other kind is sent as
// the string itself.
func convertPin(kind, raw string) (any, bool) {
	switch kind {
	case "object", "array", "null":
		return nil, false
	case "integer":
		n, err := strconv.ParseInt(raw, 10, 64)

		return n, err == nil
	case "number":
		f, err := strconv.ParseFloat(raw, 64)

		return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	case "boolean":
		switch raw {
		case "true":
			return true, true
		case "false":
			return false, true
		}

		return nil, false
	default:
		return raw, true
	}
}

func article(kind string) string {
	if kind == "integer" || kind == "object" || kind == "array" {
		return "an"
	}

	return "a"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
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
