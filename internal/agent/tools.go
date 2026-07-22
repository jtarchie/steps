package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/template"
)

// maxToolOutputBytes caps how much of a tool's textual output (file
// contents, command stdout/stderr) is handed back to the model. A runaway
// command (cat a huge file, find /) would otherwise buffer megabytes into
// memory and flood the model's context window (cost, and possible
// context-limit failures). Anything beyond this is truncated with a
// trailing marker so the model knows output was cut.
const maxToolOutputBytes = 100_000

// toolEnv is the execution environment tool impls run against: the step's
// working directory (also the bind-mount source when runner is a
// DockerRunner, so host-side tools like read_file/list_dir keep seeing what
// a containerized run_shell/custom tool wrote) and the runner shell-backed
// tools execute commands through.
type toolEnv struct {
	dir    string
	runner shell.Runner
}

// toolImpl executes one resolved tool against env, given the model's args.
// It returns the map sent back as the FunctionResponse — never a Go error;
// every failure (including a required: true tool's) is reported to the
// model as data ({"error": ...} or a nonzero "exit_code") so it can react on
// its next turn instead of the whole attempt being aborted and restarted.
// See runAgentConversation for how required: true is actually enforced —
// by tracking success and, if the model tries to stop early, forcing
// another call via forceRequiredTool — rather than by a tool ever failing
// the step directly.
type toolImpl func(ctx context.Context, args map[string]any, env toolEnv) map[string]any

// agentTools bundles what buildAgentTools produced: the declarations sent
// to the model, the registry used to execute the calls it makes, the subset
// of tool names marked required: true (see requiredToolNames), and any
// per-tool max_calls: budgets (see maxCallsByName).
type agentTools struct {
	decls    *genai.Tool
	registry map[string]toolImpl
	required map[string]bool
	maxCalls map[string]int
}

// requiredToolNames returns the set of tool names in specs marked
// required: true. A required tool's failure already aborts the attempt (see
// execCustomTool), but that alone doesn't guarantee the model ever called it
// — it could just as easily finish the conversation without calling it at
// all and still have the step report success. runAgentConversation uses
// this set to reject that case: every required tool must be invoked (and,
// since a failed call already aborts the attempt, therefore succeed) at
// least once before an attempt can end successfully.
func requiredToolNames(specs []config.ToolSpec) map[string]bool {
	required := make(map[string]bool, len(specs))

	for _, spec := range specs {
		if spec.Required {
			required[config.ToolSpecName(spec)] = true
		}
	}

	return required
}

// maxCallsByName returns, for every custom tool spec carrying a max_calls: >
// 0 budget, that budget keyed by the tool's name. A tool absent from the
// result has no budget (unlimited) — see the per-attempt call counter in
// runAgentConversation, which enforces this before a call reaches toolImpl.
func maxCallsByName(specs []config.ToolSpec) map[string]int {
	budgets := make(map[string]int, len(specs))

	for _, spec := range specs {
		if spec.MaxCalls > 0 {
			budgets[config.ToolSpecName(spec)] = spec.MaxCalls
		}
	}

	return budgets
}

type builtinTool struct {
	decl *genai.FunctionDeclaration
	impl toolImpl
}

// runShellDescription is run_shell's base description, extended when the
// step is containerized (image != "") so the model knows each call is a
// fresh, independent container: a cd/env var/installed package from one
// run_shell call is invisible to the next, unlike host execution where
// state persists naturally across calls in the same conversation.
func runShellDescription(image string) string {
	desc := "Run a shell command via `sh -c`, with cwd set to the step's working directory. Returns stdout, stderr, and exit_code."
	if image != "" {
		desc += " Runs in a fresh, independent container each call — nothing installed, exported, or cd'd in one call persists to the next; chain related commands with && in a single call instead of relying on state from a prior one."
	}

	return desc
}

func builtinAgentTools(image string) map[string]builtinTool {
	return map[string]builtinTool{
		"read_file": {
			decl: &genai.FunctionDeclaration{
				Name:        "read_file",
				Description: "Read a UTF-8 text file's contents, given a path relative to the step's working directory.",
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "File path, relative to the working directory."}},
					Required:   []string{"path"},
				},
			},
			impl: execReadFile,
		},
		"list_dir": {
			decl: &genai.FunctionDeclaration{
				Name:        "list_dir",
				Description: `List entries (name, is_dir, size) in a directory, given a path relative to the working directory. Defaults to "." if omitted.`,
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"path": {Type: genai.TypeString, Description: "Directory path, relative to the working directory."}},
				},
			},
			impl: execListDir,
		},
		"run_shell": {
			decl: &genai.FunctionDeclaration{
				Name:        "run_shell",
				Description: runShellDescription(image),
				Parameters: &genai.Schema{
					Type:       genai.TypeObject,
					Properties: map[string]*genai.Schema{"command": {Type: genai.TypeString, Description: "Command to run via sh -c."}},
					Required:   []string{"command"},
				},
			},
			impl: execRunShell,
		},
	}
}

// buildAgentTools turns a step's resolved tools: list into the genai
// declarations sent to the model, a name -> toolImpl execution registry,
// and the connections (currently: MCP servers, one per granted server —
// see buildMCPTools) that must be closed once the step ends. An empty
// specs enables every built-in. A duplicate tool name (built-in vs custom
// vs sub-agent vs MCP) is an error, so the model never sees an ambiguous
// function set — and, since resolving a later spec might still fail after
// an earlier MCP spec already opened a connection, every closer collected
// so far is closed before returning any error, so a failed step
// preparation never leaks a connection. image is the step's resolved image
// (empty for host execution), used only to adjust run_shell's description
// — see runShellDescription. cfg is needed to resolve any sub-agent or MCP
// tools; it may be nil only where the caller guarantees neither is present
// (e.g. RunFix, since a fix agent's grant may not include sub-agents —
// validateFixAgentSubAgents — though it may include MCP tools, so cfg must
// be non-nil whenever an MCP grant is possible).
func buildAgentTools(ctx context.Context, cfg *config.Config, specs []config.ToolSpec, image string) (*genai.Tool, map[string]toolImpl, []io.Closer, error) {
	if len(specs) == 0 {
		specs = config.DefaultAgentToolSpecs()
	}

	builtins := builtinAgentTools(image)
	decls := make([]*genai.FunctionDeclaration, 0, len(specs))
	registry := make(map[string]toolImpl, len(specs))

	var closers []io.Closer

	for _, spec := range specs {
		if spec.MCP != "" {
			mcpDecls, mcpImpls, closer, err := buildMCPTools(ctx, cfg, spec)
			if err != nil {
				closeAll(closers)

				return nil, nil, nil, err
			}

			closers = append(closers, closer)

			for _, decl := range mcpDecls {
				if _, exists := registry[decl.Name]; exists {
					closeAll(closers)

					return nil, nil, nil, fmt.Errorf("duplicate tool name %q", decl.Name)
				}

				decls = append(decls, decl)
				registry[decl.Name] = mcpImpls[decl.Name]
			}

			continue
		}

		decl, impl, closer, err := resolveToolSpec(ctx, cfg, spec, builtins)
		if err != nil {
			closeAll(closers)

			return nil, nil, nil, err
		}

		if closer != nil {
			closers = append(closers, closer)
		}

		if _, exists := registry[decl.Name]; exists {
			closeAll(closers)

			return nil, nil, nil, fmt.Errorf("duplicate tool name %q", decl.Name)
		}

		decls = append(decls, decl)
		registry[decl.Name] = impl
	}

	return &genai.Tool{FunctionDeclarations: decls}, registry, closers, nil
}

// closeAll closes every closer, ignoring individual errors — used only on
// an error path where step preparation is already failing and a close
// failure has nothing useful to add.
func closeAll(closers []io.Closer) {
	for _, c := range closers {
		_ = c.Close()
	}
}

// multiCloser closes every closer it holds, joining any errors — the single
// io.Closer a sub-agent tool returns for its (possibly several, if the
// child itself grants multiple MCP servers) own closers, so the parent's
// closers list stays flat (one entry per top-level spec) regardless of how
// many connections a given spec transitively opened.
type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var errs []error

	for _, c := range m {
		err := c.Close()
		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func resolveToolSpec(ctx context.Context, cfg *config.Config, spec config.ToolSpec, builtins map[string]builtinTool) (*genai.FunctionDeclaration, toolImpl, io.Closer, error) {
	if spec.Agent != "" {
		return buildSubAgentTool(ctx, cfg, spec)
	}

	if spec.Builtin != "" {
		bt, ok := builtins[spec.Builtin]
		if !ok {
			return nil, nil, nil, fmt.Errorf("unknown builtin tool %q", spec.Builtin)
		}

		return bt.decl, bt.impl, nil, nil
	}

	if spec.Name == "" || spec.Run == "" {
		return nil, nil, nil, errors.New("agent tool: custom tool requires both name and run")
	}

	params := inferToolParams(spec.Run)
	schemaParams := visibleParams(params, spec.Args)
	properties := make(map[string]*genai.Schema, len(schemaParams))

	for _, p := range schemaParams {
		properties[p] = &genai.Schema{Type: genai.TypeString}
	}

	decl := &genai.FunctionDeclaration{
		Name:        spec.Name,
		Description: spec.Description,
		Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: properties, Required: schemaParams},
	}

	return decl, execCustomTool(spec, params), nil, nil
}

// visibleParams returns params minus any key pinned by spec.Args: a pinned
// key is excluded from the schema shown to the model entirely (not merely
// optional), since the model can neither see nor override it — see
// mergePinnedArgs. Template rendering still needs the full params list
// (passed separately to execCustomTool), since a pinned value is always
// present at execution regardless of what the model supplied.
func visibleParams(params []string, pinned map[string]string) []string {
	if len(pinned) == 0 {
		return params
	}

	visible := make([]string, 0, len(params))

	for _, p := range params {
		if _, isPinned := pinned[p]; !isPinned {
			visible = append(visible, p)
		}
	}

	return visible
}

// agentToolArgPattern matches a {{ .args.NAME }} reference and everything up
// to the closing "}}", so it also matches the project's own documented safe
// idiom for passing a model-supplied value through a pipeline function —
// {{ .args.repo | shellquote }} (see CLAUDE.md's Template Rendering
// section and examples/agents.yml's post_review tool) — not just the bare
// form. [^}]* deliberately doesn't try to parse the pipeline itself (a
// function name, further pipe stages, quoted literal arguments); it only
// needs to not stop matching before the "}}" that ends the reference. A tool
// written the documented safe way must still have its argument inferred
// (and therefore checked as present/schema'd for the model), or the
// project's own recommended mitigation for missing-argument validation is
// silently defeated for exactly the tools that follow it.
//
//nolint:gochecknoglobals // compiled once, read-only
var agentToolArgPattern = regexp.MustCompile(`\{\{-?\s*\.args\.([A-Za-z_]\w*)[^}]*\}\}`)

// inferToolParams scans a custom tool's run template for {{ .args.NAME }}
// references (including {{ .args.NAME | shellquote }} and similar piped
// forms), returning each distinct NAME once, in first-seen order.
func inferToolParams(run string) []string {
	matches := agentToolArgPattern.FindAllStringSubmatch(run, -1)

	seen := make(map[string]bool, len(matches))
	params := make([]string, 0, len(matches))

	for _, m := range matches {
		name := m[1]
		if seen[name] {
			continue
		}

		seen[name] = true

		params = append(params, name)
	}

	return params
}

// resolveAgentPath resolves rel (as given by the model) against dir and
// rejects any result that escapes dir — lexically (a crafted
// "../../etc/passwd" style path) and, once a target actually exists, by
// symlink (see rejectSymlinkEscape) — so it can't be used to read/list
// outside the step's working directory.
func resolveAgentPath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative", rel)
	}

	resolved := filepath.Clean(filepath.Join(dir, rel))
	base := filepath.Clean(dir)

	if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the working directory", rel)
	}

	err := rejectSymlinkEscape(base, resolved, rel)
	if err != nil {
		return "", err
	}

	return resolved, nil
}

// rejectSymlinkEscape re-validates resolved (already confined lexically by
// resolveAgentPath) against every symlink actually present on disk: the
// lexical check is a pure string comparison (filepath.Clean + HasPrefix), so
// it's satisfied by "dir/leak" even when leak is a symlink pointing anywhere
// on the host — planted, for instance, via run_shell (which has no path
// confinement of its own) running `ln -s /etc/passwd leak` before a
// read_file("leak") call. EvalSymlinks resolves every symlink in resolved
// (mirroring shell/docker.go's resolveMountPath for the docker bind-mount
// path) and the result is re-checked against dir's own resolved form, so a
// symlink escaping dir is rejected instead of silently followed.
//
// A resolved path that does not yet exist is not treated as an escape:
// read_file/list_dir will fail with their own "not found" error when they
// try to open it, exactly as before this check existed, and there is
// nothing to leak from a path with no target.
func rejectSymlinkEscape(base, resolved, rel string) error {
	realResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("%w", err)
	}

	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if realResolved != realBase && !strings.HasPrefix(realResolved, realBase+string(os.PathSeparator)) {
		return fmt.Errorf("path %q escapes the working directory (resolves to %q via a symlink)", rel, realResolved)
	}

	return nil
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)

	return v
}

// truncateToolOutput caps s at maxToolOutputBytes, appending a marker when
// it cuts, so a runaway command can't flood the model's context.
func truncateToolOutput(s string) string {
	if len(s) <= maxToolOutputBytes {
		return s
	}

	return s[:maxToolOutputBytes] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-maxToolOutputBytes)
}

// shellToolResult builds the FunctionResponse map for a shell-backed tool
// (run_shell and every custom tool). It executes command through env.runner
// — the host or, when the step's image: is set, a fresh container — with
// env.dir as cwd, via RunCaptureFullLimited so a runaway command's output is
// capped as it's captured rather than fully buffered and truncated
// afterward.
func shellToolResult(ctx context.Context, command string, env toolEnv) map[string]any {
	stdout, stderr, exitCode, err := env.runner.RunCaptureFullLimited(ctx, command, maxToolOutputBytes)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{
		"stdout":    stdout,
		"stderr":    stderr,
		"exit_code": exitCode,
	}
}

func execReadFile(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `read_file: missing required argument "path"`}
	}

	resolved, err := resolveAgentPath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	f, err := os.Open(resolved) //nolint:gosec // resolveAgentPath rejects paths escaping dir, the step's own workspace
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	// Read at most maxToolOutputBytes regardless of the file's real size, so a
	// huge file (a multi-GB log accidentally left in the working tree) doesn't
	// pay full allocation and I/O cost before truncateToolOutput would have
	// discarded most of it anyway. stat.Size() (not the read length) drives the
	// truncation marker, so the reported byte count matches what
	// truncateToolOutput would have said had it read the whole file itself.
	data, err := io.ReadAll(io.LimitReader(f, maxToolOutputBytes))
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	content := string(data)
	if stat.Size() > maxToolOutputBytes {
		content += fmt.Sprintf("\n... [truncated %d bytes]", stat.Size()-maxToolOutputBytes)
	}

	return map[string]any{"content": content}
}

func execListDir(_ context.Context, args map[string]any, env toolEnv) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		rel = "."
	}

	resolved, err := resolveAgentPath(env.dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	items := make([]map[string]any, 0, len(entries))

	for _, e := range entries {
		size := int64(0)

		info, infoErr := e.Info()
		if infoErr == nil {
			size = info.Size()
		}

		items = append(items, map[string]any{"name": e.Name(), "is_dir": e.IsDir(), "size": size})
	}

	return map[string]any{"entries": items}
}

func execRunShell(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	command := stringArg(args, "command")
	if command == "" {
		return map[string]any{"error": `run_shell: missing required argument "command"`}
	}

	return shellToolResult(ctx, command, env)
}

// execCustomTool renders spec.Run against the model's args (with spec.Args
// pinned values merged over them — see mergePinnedArgs) and shells it out.
// Model-supplied arg values are interpolated into the sh -c string, so a
// custom tool is a capability-curation convenience, not a hard sandbox — the
// same trust boundary as run_shell itself. A run: template should pipe each
// model-supplied value through the shellquote function (see
// internal/template) so shell metacharacters in the value are passed through
// literally rather than interpreted; a template that doesn't is as trusting
// of the model's output as run_shell is. A required: true tool is not
// special-cased here: its nonzero exit is reported as ordinary data, same as
// any other tool, so the model can see what went wrong and recover on its
// next turn. required: is enforced by runAgentConversation tracking success
// and forcing another call if the model tries to stop early — never by a
// tool failing the step directly. A max_calls: budget is enforced one layer
// up, in the conversation loop (see maxCallsByName), before this impl is
// ever invoked — a rejected call never reaches here.
func execCustomTool(spec config.ToolSpec, params []string) toolImpl {
	return func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
		merged := mergePinnedArgs(args, spec.Args)

		missing := missingArgs(merged, params)
		if len(missing) > 0 {
			return map[string]any{"error": fmt.Sprintf("%s: missing required argument(s): %s", spec.Name, strings.Join(missing, ", "))}
		}

		rendered, err := template.Render(spec.Run, map[string]any{"args": merged})
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		return shellToolResult(ctx, rendered, env)
	}
}

// mergePinnedArgs returns a copy of args with spec's pinned values merged
// OVER any model-supplied value at the same key — pinned always wins, and
// (per visibleParams) the model never even sees a pinned key in its schema,
// so this only ever overrides a value the model couldn't have legitimately
// supplied. A nil/empty pinned map returns args unchanged (no copy).
func mergePinnedArgs(args map[string]any, pinned map[string]string) map[string]any {
	if len(pinned) == 0 {
		return args
	}

	merged := make(map[string]any, len(args)+len(pinned))

	for k, v := range args {
		merged[k] = v
	}

	for k, v := range pinned {
		merged[k] = v
	}

	return merged
}

// missingArgs returns the subset of params for which args holds no non-empty
// string value, in params order — so a custom tool can report every missing
// argument in one message instead of the model discovering them one failed
// render at a time.
func missingArgs(args map[string]any, params []string) []string {
	missing := make([]string, 0, len(params))

	for _, p := range params {
		if stringArg(args, p) == "" {
			missing = append(missing, fmt.Sprintf("%q", p))
		}
	}

	return missing
}

func executeAgentTool(ctx context.Context, call *genai.FunctionCall, env toolEnv, registry map[string]toolImpl) map[string]any {
	impl, ok := registry[call.Name]
	if !ok {
		return map[string]any{"error": fmt.Sprintf("unknown tool %q", call.Name)}
	}

	return impl(ctx, call.Args, env)
}
