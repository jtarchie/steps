package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/template"
)

// callMCPResourceTool connects to the mcp_servers: entry named serverName
// (fresh per call — no connection pooling, mirroring how shell.NewRunner is
// built fresh per check/in/out command today), calls toolName with args,
// and closes the connection before returning. A CallToolResult with
// IsError: true is turned into a Go error here (unlike internal/agent's
// "failure is data" toolImpl convention) — a resource step has no model to
// hand a soft failure to react to; it's a hard failure like a nonzero
// shell exit.
func callMCPResourceTool(ctx context.Context, cfg *config.Config, serverName, toolName string, args map[string]any) (*sdkmcp.CallToolResult, error) {
	srv, err := cfg.FindMCPServer(serverName)
	if err != nil {
		return nil, err //nolint:wrapcheck // caller (mcpCheckVersions/mcpRunIn/mcpRunOut) wraps with resource-type context
	}

	client, err := stepsmcp.Connect(ctx, *srv)
	if err != nil {
		return nil, err //nolint:wrapcheck // caller wraps with resource-type context
	}
	defer func() { _ = client.Close() }()

	result, err := client.CallTool(ctx, toolName, args)
	if err != nil {
		return nil, err //nolint:wrapcheck // caller wraps with resource-type context
	}

	if result.IsError {
		return nil, fmt.Errorf("mcp tool %q returned an error: %s", toolName, firstTextContent(result.Content))
	}

	return result, nil
}

// mcpCallArgs builds the argument object one lifecycle stage calls its tool
// with: call.Args when set, with every string leaf rendered as a template
// over data (whatever that stage has to template against — {source} for
// check, {source, params} for out, {source, version, params} for in, exactly
// mirroring the shell path's command templates); otherwise fallback,
// verbatim.
//
// The arguments ARE the remote tool's own published schema — see
// config.MCPToolCall — so this returns exactly what the author named and
// wraps it in nothing.
func mcpCallArgs(call config.MCPToolCall, data, fallback map[string]any) (map[string]any, error) {
	if call.Args == nil {
		return fallback, nil
	}

	rendered, err := renderArgValue("args", call.Args, exactNumberData(data))
	if err != nil {
		return nil, err
	}

	args, ok := rendered.(map[string]any)
	if !ok {
		// renderArgValue preserves shape, so a map in is a map out; this is
		// unreachable, and returning the un-rendered args would be worse.
		return nil, fmt.Errorf("args for tool %q did not render as an object", call.Tool)
	}

	return args, nil
}

// renderArgValue renders every string leaf of an args: value, recursing
// through maps and slices so a template works at whatever depth the remote
// tool's schema nests to. Non-string scalars (numbers, booleans, null) pass
// through untouched — a `limit: 20` is a number to the tool, not a string.
//
// path names the leaf being rendered (`args.channel_id`, `args.include[0]`)
// so a failed template says WHICH one, and is wrapped exactly once — at the
// leaf that produced the error rather than again at every level above it.
func renderArgValue(path string, value any, data map[string]any) (any, error) {
	switch typed := value.(type) {
	case string:
		rendered, err := template.Render(typed, data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		return rendered, nil
	case map[string]any:
		return renderArgMap(path, typed, data)
	case map[any]any:
		// yaml.v3 decodes a mapping whose keys are not ALL strings into
		// map[any]any — an unquoted `on:`, a filter keyed by a year. Left in
		// that shape it is both un-rendered (its templates would reach the
		// server as the literal text `{{ .version.ts }}`) and un-marshalable
		// by encoding/json, so normalize the keys and treat it as the object
		// the author obviously meant.
		normalized := make(map[string]any, len(typed))
		for key, inner := range typed {
			normalized[fmt.Sprint(key)] = inner
		}

		return renderArgMap(path, normalized, data)
	case []any:
		out := make([]any, len(typed))

		for i, inner := range typed {
			converted, err := renderArgValue(fmt.Sprintf("%s[%d]", path, i), inner, data)
			if err != nil {
				return nil, err
			}

			out[i] = converted
		}

		return out, nil
	default:
		return value, nil
	}
}

// renderArgMap renders every value of one args: object, keyed path-wise.
func renderArgMap(path string, value, data map[string]any) (any, error) {
	out := make(map[string]any, len(value))

	for key, inner := range value {
		converted, err := renderArgValue(path+"."+key, inner, data)
		if err != nil {
			return nil, err
		}

		out[key] = converted
	}

	return out, nil
}

// exactNumberData returns a copy of a stage's template data with every
// float64 leaf replaced by a json.Number holding its exact decimal spelling.
//
// A version object reaches here through encoding/json, so every number in it
// is a float64 — and text/template prints a float64 with %v, which switches
// to exponent form well inside the range of ordinary identifiers. Without
// this, the documented `issue_id: "{{ .version.id }}"` sends
// "1.23456789e+08" for issue 123456789, and a Slack `message_ts` goes out as
// "1.717171717123456e+09". No server accepts either. json.Number is a string
// type, so a template prints its digits verbatim.
func exactNumberData(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))

	for key, value := range data {
		out[key] = exactNumbers(value)
	}

	return out
}

func exactNumbers(value any) any {
	switch typed := value.(type) {
	case float64:
		return json.Number(strconv.FormatFloat(typed, 'f', -1, 64))
	case map[string]any:
		return exactNumberData(typed)
	case []any:
		out := make([]any, len(typed))
		for i, inner := range typed {
			out[i] = exactNumbers(inner)
		}

		return out
	default:
		return value
	}
}

// mcpCheckVersions calls rt.Config.MCP.Check.Tool with the resource's
// source: as its arguments (or with check.args:, rendered — see
// mcpCallArgs), and parses the result the same way CheckVersions's doc
// comment documents for the shell path: an oldest-first JSON array of
// version objects, accepted either as the tool result's StructuredContent or
// a single text content block containing that same JSON array.
//
// check: is optional (a publish-only type declares none — see
// config.validateResourceGet), and a get against such a type is a load error,
// so this is only ever reached with one set. The guard is there because
// "unreachable" and "nil dereference" are one refactor apart.
func mcpCheckVersions(ctx context.Context, cfg *config.Config, rt config.ResourceType, source map[string]any) ([]map[string]any, error) {
	if rt.Config.MCP.Check == nil {
		return nil, fmt.Errorf("check %q: this resource type sets no mcp.check.tool, so it can only be published to", rt.Name)
	}

	slog.Debug("resource.check", "resource_type", rt.Name, "source", source, "mcp_tool", rt.Config.MCP.Check.Tool)

	args, err := mcpCallArgs(*rt.Config.MCP.Check, map[string]any{"source": source}, source)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	result, err := callMCPResourceTool(ctx, cfg, rt.Config.MCP.Server, rt.Config.MCP.Check.Tool, args)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	versions, err := parseVersionArray(result)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	slog.Info("resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

// parseVersionArray extracts an oldest-first []map[string]any from result:
// StructuredContent when it round-trips into that shape (normalizing plain
// maps regardless of its concrete Go shape), else the first text content
// block that parses as one — trying each block in turn, since a tool may
// emit a human-readable block before (or after) the JSON one, not just a
// single block. Returns an error only when neither source yields a usable
// array — e.g. StructuredContent is present but object-shaped (not an
// array) and no text block parses as one either.
func parseVersionArray(result *sdkmcp.CallToolResult) ([]map[string]any, error) {
	if result.StructuredContent != nil {
		versions, err := convertToMapSlice(result.StructuredContent)
		if err == nil {
			return versions, nil
		}
	}

	versions, ok := firstParsableArray(result.Content)
	if !ok {
		return nil, errors.New("mcp tool returned no structured content and no text content block that parses as a JSON array")
	}

	return versions, nil
}

// firstParsableArray scans content in order and returns the first text
// block that parses as a []map[string]any, so a check/out tool can freely
// interleave prose with its JSON payload.
func firstParsableArray(content []sdkmcp.Content) ([]map[string]any, bool) {
	for _, c := range content {
		tc, ok := c.(*sdkmcp.TextContent)
		if !ok {
			continue
		}

		var versions []map[string]any

		err := json.Unmarshal([]byte(tc.Text), &versions)
		if err == nil {
			return versions, true
		}
	}

	return nil, false
}

// convertToMapSlice round-trips sc through JSON to normalize it into
// []map[string]any regardless of its concrete Go shape ([]any of
// map[string]any from a real JSON-RPC decode, or already []map[string]any
// from a hand-built test fixture).
func convertToMapSlice(sc any) ([]map[string]any, error) {
	data, err := json.Marshal(sc)
	if err != nil {
		return nil, fmt.Errorf("could not marshal structured content: %w", err)
	}

	var versions []map[string]any

	err = json.Unmarshal(data, &versions)
	if err != nil {
		return nil, fmt.Errorf("structured content is not a JSON array of objects: %w", err)
	}

	return versions, nil
}

func firstTextContent(content []sdkmcp.Content) string {
	for _, c := range content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			return tc.Text
		}
	}

	return ""
}

// mcpRunIn implements RunIn's mcp:-backed path. The selected version object
// is always written as JSON to <destDir>/version.json, regardless of
// whether In is set — so a job can always rely on it being there. When
// rt.Config.MCP.In is nil, that's the whole job: no MCP call, which is all
// the common detect-and-notify case (the motivating Linear trigger use
// case) needs. When In is set, it additionally calls that tool — with the
// resource's source: as the argument object, or with in.args: rendered over
// {source, version, params}; see mcpCallArgs — and materializes the result
// into destDir (see materializeContent), a fixed convention, since unlike an
// arbitrary shell script an MCP result is a small fixed set of content
// blocks, not a tree the tool itself writes.
func mcpRunIn(ctx context.Context, cfg *config.Config, rt config.ResourceType, source, version, params map[string]any, destDir string) error {
	slog.Debug("resource.in", "resource_type", rt.Name, "source", source, "version", version, "params", params, "dest_dir", destDir)

	err := writeJSONFile(filepath.Join(destDir, "version.json"), version)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	if rt.Config.MCP.In == nil {
		slog.Info("resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

		return nil
	}

	// source verbatim when in: names no args:, which is only right for a tool
	// whose arguments the source already spells. The fields an in: usually
	// needs — which thread, which issue — live on the VERSION, and reaching
	// them is what args: is for.
	args, err := mcpCallArgs(
		*rt.Config.MCP.In,
		map[string]any{"source": source, "version": version, "params": params},
		source,
	)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	result, err := callMCPResourceTool(ctx, cfg, rt.Config.MCP.Server, rt.Config.MCP.In.Tool, args)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	err = materializeContent(result, destDir)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	slog.Info("resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

	return nil
}

// materializeContent writes an mcp in: tool's result into destDir:
// StructuredContent, if present, as result.json; each content block as
// content-N.<ext> (text as .txt; image/audio as their raw bytes, with an
// extension derived from the block's MIME type when recognized, else
// .bin; anything else as a best-effort content-N.json).
func materializeContent(result *sdkmcp.CallToolResult, destDir string) error {
	if result.StructuredContent != nil {
		err := writeJSONFile(filepath.Join(destDir, "result.json"), result.StructuredContent)
		if err != nil {
			return err
		}
	}

	for i, c := range result.Content {
		err := writeContentBlock(destDir, i, c)
		if err != nil {
			return err
		}
	}

	return nil
}

func writeContentBlock(destDir string, index int, c sdkmcp.Content) error {
	switch v := c.(type) {
	case *sdkmcp.TextContent:
		return writeFile(filepath.Join(destDir, fmt.Sprintf("content-%d.txt", index)), []byte(v.Text))
	case *sdkmcp.ImageContent:
		return writeFile(filepath.Join(destDir, fmt.Sprintf("content-%d%s", index, extensionForMIMEType(v.MIMEType))), v.Data)
	case *sdkmcp.AudioContent:
		return writeFile(filepath.Join(destDir, fmt.Sprintf("content-%d%s", index, extensionForMIMEType(v.MIMEType))), v.Data)
	default:
		data, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return fmt.Errorf("content block %d: %w", index, err)
		}

		return writeFile(filepath.Join(destDir, fmt.Sprintf("content-%d.json", index)), data)
	}
}

// extensionForMIMEType returns a file extension for mimeType via the
// standard library's registered MIME types, falling back to ".bin" when
// mimeType is empty or unrecognized.
func extensionForMIMEType(mimeType string) string {
	if mimeType == "" {
		return ".bin"
	}

	exts, err := mime.ExtensionsByType(mimeType)
	if err != nil || len(exts) == 0 {
		return ".bin"
	}

	return exts[0]
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return writeFile(path, data)
}

func writeFile(path string, data []byte) error {
	err := os.WriteFile(path, data, 0o600)
	if err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}

	return nil
}

// mcpRunOut implements RunOut's mcp:-backed path — the deterministic put:
// calls rt.Config.MCP.Out.Tool with the put's own params: as the argument
// object (symmetric with how check uses source:), or with out.args: rendered
// over {source, params} — see mcpCallArgs — and parses the result into the
// produced version object the same way mcpCheckVersions parses one array
// element: StructuredContent, or a single text content block of JSON.
//
// A shell out: runs with cwd = srcDir and reads the workspace itself; an
// MCP out: is a tool call with no working directory, so a {file: path}
// marker in params is how a value that a previous step WROTE reaches the
// tool — see resolveParamFiles. Without it the payload could only ever be
// what the pipeline author typed, which rules out posting anything an
// agent produced.
func mcpRunOut(ctx context.Context, cfg *config.Config, rt config.ResourceType, source, params map[string]any, srcDir string) (map[string]any, error) {
	slog.Debug("resource.out", "resource_type", rt.Name, "source", source, "params", params, "mcp_tool", rt.Config.MCP.Out.Tool, "src_dir", srcDir)

	resolved, err := resolveParamFiles(params, srcDir)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	// The put's params: are the payload, so they are the arguments when out:
	// names no args:. A tool whose parameter names differ from the ones the
	// put step spells (or one that needs a source field too) gets an args:
	// mapping instead.
	args, err := mcpCallArgs(
		*rt.Config.MCP.Out,
		map[string]any{"source": source, "params": resolved},
		resolved,
	)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	result, err := callMCPResourceTool(ctx, cfg, rt.Config.MCP.Server, rt.Config.MCP.Out.Tool, args)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	version := parseVersionObject(result)

	slog.Info("resource.put", "resource_type", rt.Name, "result", version)

	return version, nil
}

// paramFileKey is the sole key that turns a params: mapping into a
// reference to a workspace file rather than a literal object.
const paramFileKey = "file"

// resolveParamFiles walks params and replaces every {file: <path>} marker
// with that file's contents, read from srcDir — the put's read view,
// composed from its inputs:, so the path names a declared artifact exactly
// as everywhere else in the DSL.
//
// A mapping qualifies ONLY when `file` is its single key and its value is a
// string. That strictness is the whole safety argument for spelling this
// inside params instead of beside it: an MCP tool whose parameter genuinely
// is an object with a `file` field alongside others (say {file, title})
// keeps passing through untouched, so adding this feature cannot silently
// reinterpret a payload that already worked.
//
// Contents are TRIMMED, exactly as load_var: trims (see pipeline/vars.go):
// two features that read a file into a value must agree, and the common way
// to produce one is a redirect (`jq -r .id > meta/id`) whose trailing
// newline is an artifact of the writing, not part of the value. Untrimmed,
// the first real use — an id or timestamp handed to an API — breaks.
func resolveParamFiles(params map[string]any, srcDir string) (map[string]any, error) {
	resolved, err := resolveParamValue(params, srcDir)
	if err != nil {
		return nil, err
	}

	converted, ok := resolved.(map[string]any)
	if !ok {
		return params, nil
	}

	return converted, nil
}

// resolveParamValue recurses through maps and slices so a marker works at
// any depth a tool's argument schema nests to, not just the top level.
func resolveParamValue(value any, srcDir string) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		path, isMarker := paramFileMarker(typed)
		if isMarker {
			return readParamFile(path, srcDir)
		}

		out := make(map[string]any, len(typed))

		for key, inner := range typed {
			converted, err := resolveParamValue(inner, srcDir)
			if err != nil {
				return nil, err
			}

			out[key] = converted
		}

		return out, nil
	case []any:
		out := make([]any, len(typed))

		for i, inner := range typed {
			converted, err := resolveParamValue(inner, srcDir)
			if err != nil {
				return nil, err
			}

			out[i] = converted
		}

		return out, nil
	default:
		return value, nil
	}
}

// paramFileMarker reports whether m is a {file: <string>} marker, and the
// path it names.
func paramFileMarker(m map[string]any) (string, bool) {
	if len(m) != 1 {
		return "", false
	}

	raw, ok := m[paramFileKey]
	if !ok {
		return "", false
	}

	path, ok := raw.(string)

	return path, ok
}

// readParamFile reads one marker's file, confined to srcDir. The path rules
// mirror across:'s from_file: — relative, non-escaping, and naming an
// artifact as its first component — because it is the same kind of path:
// one pointing into a step's own materialized view.
func readParamFile(declared, srcDir string) (string, error) {
	cleaned := path.Clean(declared)

	switch {
	case declared == "":
		return "", errors.New("params file reference is empty; it must be a path inside an artifact, like answer/reply.md")
	case path.IsAbs(declared):
		return "", fmt.Errorf("params file %q is absolute; it must be a path inside an artifact, like answer/reply.md", declared)
	case cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../"):
		return "", fmt.Errorf("params file %q escapes the workspace; it must be a path inside an artifact, like answer/reply.md", declared)
	case !strings.Contains(cleaned, "/"):
		return "", fmt.Errorf("params file %q names no artifact; the first path component is the artifact holding the file, as in answer/reply.md", declared)
	}

	data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(cleaned))) //nolint:gosec // confined above, joined under the put's own read view
	if err != nil {
		return "", fmt.Errorf("params file %q: %w (is its artifact in the put's inputs:?)", declared, err)
	}

	return strings.TrimSpace(string(data)), nil
}

// parseVersionObject extracts the produced version object from an out:
// tool's result, mirroring RunOut's own shell-path convention: unparsable
// or absent content is not an error, just a nil result (many out tools
// won't produce a version-shaped response). Like parseVersionArray, it
// tries StructuredContent first, then scans every text block (not just the
// first) for one that parses as an object.
func parseVersionObject(result *sdkmcp.CallToolResult) map[string]any {
	if result.StructuredContent != nil {
		converted, err := convertToMap(result.StructuredContent)
		if err == nil {
			return converted
		}
	}

	obj, _ := firstParsableObject(result.Content)

	return obj
}

// firstParsableObject scans content in order and returns the first text
// block that parses as a map[string]any.
func firstParsableObject(content []sdkmcp.Content) (map[string]any, bool) {
	for _, c := range content {
		tc, ok := c.(*sdkmcp.TextContent)
		if !ok {
			continue
		}

		var obj map[string]any

		err := json.Unmarshal([]byte(tc.Text), &obj)
		if err == nil {
			return obj, true
		}
	}

	return nil, false
}

func convertToMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("could not marshal: %w", err)
	}

	var obj map[string]any

	err = json.Unmarshal(data, &obj)
	if err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}

	return obj, nil
}
