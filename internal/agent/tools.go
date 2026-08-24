package agent

// A step's tool set: turning its tools: grant into the declarations the model
// sees and the registry the conversation executes against.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/template"
)

// toolEnv is the execution environment tool impls run against: the step's
// working directory (also the bind-mount source when runner is a
// DockerRunner, so host-side tools like read_file/list_dir keep seeing what
// a containerized run_shell/custom tool wrote), the runner shell-backed
// tools execute commands through, and spillDir — a per-step temp directory
// (created and cleaned up by prepareAgentStep/RunFix) that run_shell/custom
// tool output too large to return inline is streamed to instead of being
// dropped. A zero-value spillDir (test toolEnvs, MCP/read_file/subagent
// tools, which don't use it) falls back to the shell layer's older
// truncate-and-drop behavior — see shellToolResult.
type toolEnv struct {
	dir      string
	runner   shell.Runner
	spillDir string
	// transcript is the enclosing conversation's recorder, set by
	// runAgentConversation. It rides in the env because the env is what
	// already reaches every toolImpl — the sub-agent tool uses it to nest the
	// child conversation's transcript into the parent's. Nil outside a
	// conversation; the recorder's methods are nil-safe.
	transcript *transcriptRecorder
	// usage is the enclosing conversation's accumulator, set alongside
	// transcript and riding here for the same reason. The sub-agent tool
	// reads it to size the child's allowance out of what the parent has left,
	// and to charge the child's spend back against it. Nil outside a
	// conversation, which leaves a child on its own declared budget.
	usage *stepUsage
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

// agentTools is everything a step's grant produced: the declarations sent to
// the model, the registry used to execute the calls it makes, the subset of
// names marked required: true, and any per-tool max_calls: budgets.
type agentTools struct {
	decls    *genai.Tool
	registry map[string]toolImpl
	// required names the tools a step must actually have called. A required
	// tool's failure does NOT abort the attempt — it comes back as ordinary
	// data, same as any other tool — but nothing otherwise stops the model
	// from finishing without ever having called it and still reporting
	// success. runAgentConversation forces an unsatisfied one via the next
	// turn's tool_choice (see forceRequiredTool) whenever the model tries to
	// stop early, and fails the step only if it still never succeeds by
	// max_turns.
	required map[string]bool
	// maxCalls holds each budgeted tool's max_calls: ceiling. A tool absent
	// from the map is unlimited. Enforced by the conversation loop's
	// per-attempt counter, before a call reaches its toolImpl.
	maxCalls map[string]int
	// webFetchAllow is the web_fetch grant's allow: list, carried out of the
	// spec because the CLI path needs it AFTER resolution: a native WebFetch
	// grant expresses the list as per-domain permission entries
	// (cliToolPermissions), and by then only the declarations remain.
	webFetchAllow []string
	// builtins names the tools that came from a BUILTIN grant, as opposed to
	// a custom tool that happens to spell its name the same way. It exists
	// because the CLI runtime's natives table is keyed by builtin name, and
	// a pipeline is free to write {name: web_fetch, run: ./authfetch.sh}:
	// mapping that to the CLI's own WebFetch would substitute a different
	// capability for the one the pipeline wrote, silently. Provenance is
	// known only here, where the spec is still in hand.
	builtins map[string]bool
}

// resolvedSpec is what one tools: entry produced. A spec yields several
// declarations only in the bare-MCP case (every tool the server offers); every
// other kind yields exactly one.
type resolvedSpec struct {
	decls  []*genai.FunctionDeclaration
	impls  map[string]toolImpl
	closer io.Closer
}

// buildAgentTools turns a step's resolved tools: list into the declarations
// sent to the model, the name -> toolImpl execution registry, and the
// connections (currently: MCP servers, one per granted server) that must be
// closed once the step ends. An empty specs enables the read-only default set
// (config.DefaultAgentToolSpecs).
//
// A duplicate tool name (built-in vs custom vs sub-agent vs MCP) is an error,
// so the model never sees an ambiguous function set — and, since resolving a
// later spec might still fail after an earlier MCP spec already opened a
// connection, every closer collected so far is closed before returning any
// error, so a failed step preparation never leaks a connection.
//
// image is the step's resolved image (empty for host execution), used only to
// adjust run_shell's description. cfg is needed to resolve any sub-agent or
// MCP tools; it may be nil only where the caller guarantees neither is
// present (e.g. RunFix, since a fix agent's grant may not include sub-agents
// — validateFixAgentSubAgents — though it may include MCP tools, so cfg must
// be non-nil whenever an MCP grant is possible).
func buildAgentTools(ctx context.Context, cfg *config.Config, specs []config.ToolSpec, image string) (agentTools, []io.Closer, error) {
	if len(specs) == 0 {
		specs = config.DefaultAgentToolSpecs()
	}

	builtins := builtinAgentTools(image)

	tools := agentTools{
		registry: make(map[string]toolImpl, len(specs)),
		required: requiredToolNames(specs),
		maxCalls: maxCallsByName(specs),
		builtins: make(map[string]bool, len(specs)),
	}

	decls := make([]*genai.FunctionDeclaration, 0, len(specs))

	var closers []io.Closer

	for _, spec := range specs {
		if spec.Builtin != "" {
			tools.builtins[spec.Builtin] = true
		}

		if spec.Builtin == config.WebFetchBuiltinName {
			tools.webFetchAllow = spec.Allow
		}

		resolved, err := resolveToolSpec(ctx, cfg, spec, builtins)
		if err == nil {
			err = resolved.bound(spec)
		}

		if err == nil {
			if resolved.closer != nil {
				closers = append(closers, resolved.closer)
			}

			err = tools.add(resolved, &decls)
		}

		if err != nil {
			closeAll(closers)

			return agentTools{}, nil, err
		}
	}

	tools.decls = &genai.Tool{FunctionDeclarations: decls}

	return tools, closers, nil
}

// requiredToolNames is the set of tool names in specs marked required: true.
func requiredToolNames(specs []config.ToolSpec) map[string]bool {
	required := make(map[string]bool, len(specs))

	for _, spec := range specs {
		if spec.Required {
			required[config.ToolSpecName(spec)] = true
		}
	}

	return required
}

// maxCallsByName is each spec's max_calls: budget, keyed by tool name. A tool
// absent from the result is unlimited.
func maxCallsByName(specs []config.ToolSpec) map[string]int {
	budgets := make(map[string]int, len(specs))

	for _, spec := range specs {
		if spec.MaxCalls > 0 {
			budgets[config.ToolSpecName(spec)] = spec.MaxCalls
		}
	}

	return budgets
}

// add registers one spec's declarations, rejecting a name already taken.
func (t agentTools) add(resolved resolvedSpec, decls *[]*genai.FunctionDeclaration) error {
	for _, decl := range resolved.decls {
		if _, exists := t.registry[decl.Name]; exists {
			return fmt.Errorf("duplicate tool name %q", decl.Name)
		}

		*decls = append(*decls, decl)
		t.registry[decl.Name] = resolved.impls[decl.Name]
	}

	return nil
}

// resolveToolSpec resolves one tools: entry — an MCP grant, a sub-agent, a
// builtin, or a custom shell tool.
func resolveToolSpec(ctx context.Context, cfg *config.Config, spec config.ToolSpec, builtins map[string]builtinTool) (resolvedSpec, error) {
	switch {
	case spec.MCP != "":
		decls, impls, closer, err := buildMCPTools(ctx, cfg, spec)

		return resolvedSpec{decls: decls, impls: impls, closer: closer}, err

	case spec.Agent != "":
		decl, impl, closer, err := buildSubAgentTool(ctx, cfg, spec)
		if err != nil {
			return resolvedSpec{}, err
		}

		return one(decl, impl, closer), nil

	case spec.Builtin != "":
		return resolveBuiltinSpec(spec, builtins)

	case spec.Name != "" && spec.Run != "":
		decl, impl := customToolDecl(spec)

		return one(decl, impl, nil), nil

	default:
		return resolvedSpec{}, errors.New("agent tool: custom tool requires both name and run")
	}
}

// resolveBuiltinSpec resolves a builtin grant from the catalogue. web_fetch
// is the one builtin whose declaration and impl depend on the grant itself:
// allow: narrows what it may reach, and the declaration tells the model so.
func resolveBuiltinSpec(spec config.ToolSpec, builtins map[string]builtinTool) (resolvedSpec, error) {
	bt, ok := builtins[spec.Builtin]
	if !ok {
		return resolvedSpec{}, fmt.Errorf("unknown builtin tool %q", spec.Builtin)
	}

	if spec.Builtin == config.WebFetchBuiltinName && len(spec.Allow) > 0 {
		decl, impl := webFetchTool(spec.Allow)

		return one(decl, impl, nil), nil
	}

	return one(bt.decl, bt.impl, nil), nil
}

// one is the single-declaration resolvedSpec every kind but a bare MCP grant
// produces.
func one(decl *genai.FunctionDeclaration, impl toolImpl, closer io.Closer) resolvedSpec {
	return resolvedSpec{
		decls:  []*genai.FunctionDeclaration{decl},
		impls:  map[string]toolImpl{decl.Name: impl},
		closer: closer,
	}
}

// customToolDecl builds a shell-backed tool's declaration from its run:
// template — every {{ .args.NAME }} it references becomes a required string
// parameter, minus the ones spec.Args pins.
func customToolDecl(spec config.ToolSpec) (*genai.FunctionDeclaration, toolImpl) {
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

	return decl, execCustomTool(spec, params)
}

// bound wraps every impl this spec produced in its timeout:, if it set one.
// It happens here — at build time, on the impl itself — rather than in the
// conversation loop, because the loop is not the only caller: a CLI-backed
// step's bridge (see clibridge.go) hands the child the SAME impls over MCP,
// and a deadline the delegated path silently dropped would be a fence that
// exists only on one of two backends.
func (r resolvedSpec) bound(spec config.ToolSpec) error {
	if spec.Timeout == "" {
		return nil
	}

	timeout, err := config.ParseTimeout(spec.Timeout)
	if err != nil {
		return fmt.Errorf("agent tool %q: %w", config.ToolSpecName(spec), err)
	}

	if timeout <= 0 {
		return nil
	}

	for name, impl := range r.impls {
		r.impls[name] = withToolTimeout(name, timeout, impl)
	}

	return nil
}

// withToolTimeout bounds one call of impl to d, reporting an expiry to the
// model as ordinary tool-result data ({"error": ...}) — the tool contract is
// that no failure aborts an attempt, and running out of time is a failure
// like any other. Whatever the impl managed to return is preserved alongside
// it, so a partial result is not thrown away to say why it stopped.
//
// It is an "error" rather than a softer note ({"timed_out": true}) because
// error is the one channel three separate readers already agree on:
// requiredCallSucceeded (a required: tool that ran out of time has NOT been
// satisfied, and the loop must keep forcing it), the CLI bridge's IsError
// (a delegated child must see the same failure a hosted model does), and
// assert: tool_calls. A neutral key would read as success to all three.
//
// The distinction being drawn on the way out is between THIS call's deadline
// and the enclosing step's: a parent that is itself done (its own timeout:,
// or SIGINT) is an abort, and dressing it up as the tool's own deadline
// would misreport why the run ended.
//
// A tool that ignores its context — the purely local built-ins take none —
// still runs to completion; the deadline reports on it rather than
// interrupting it. Killing an impl mid-write would trade a slow tool for a
// half-written file.
func withToolTimeout(name string, d time.Duration, impl toolImpl) toolImpl {
	return func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
		callCtx, cancel := context.WithTimeout(ctx, d)
		defer cancel()

		response := impl(callCtx, args, env)

		if !errors.Is(callCtx.Err(), context.DeadlineExceeded) || ctx.Err() != nil {
			return response
		}

		if response == nil {
			response = map[string]any{}
		}

		response["error"] = fmt.Sprintf("%s: timed out after %s and was cancelled; anything it returned is partial", name, d)

		return response
	}
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
// {{ .args.repo | shellquote }} (see docs/templating.md and docs/agents.md's
// post_review tool) — not just the bare form. [^}]* deliberately doesn't try
// to parse the pipeline itself; it only needs to not stop matching before the
// "}}" that ends the reference. A tool written the documented safe way must
// still have its argument inferred (and therefore checked as present and
// schema'd for the model), or the project's own recommended mitigation for
// missing-argument validation is silently defeated for exactly the tools that
// follow it.
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

// execCustomTool renders spec.Run against the model's args (with spec.Args
// pinned values merged over them — see mergePinnedArgs) and shells it out.
// Model-supplied arg values are interpolated into the sh -c string, so a
// custom tool is a capability-curation convenience, not a hard sandbox — the
// same trust boundary as run_shell itself. A run: template should pipe each
// model-supplied value through the shellquote function (see
// internal/template) so shell metacharacters in the value are passed through
// literally rather than interpreted.
//
// A required: true tool is not special-cased here: its nonzero exit is
// reported as ordinary data, so the model can see what went wrong and recover
// on its next turn. A max_calls: budget is enforced one layer up, in the
// conversation loop, before this impl is ever invoked.
func execCustomTool(spec config.ToolSpec, params []string) toolImpl {
	return func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
		merged := mergePinnedArgs(args, spec.Args)

		missing := missingArgs(merged, params)
		if len(missing) > 0 {
			msg := fmt.Sprintf("%s: missing required argument(s): %s", spec.Name, quoteJoin(missing))

			if expected := visibleParams(params, spec.Args); len(expected) > 0 {
				msg += fmt.Sprintf(" (expected: %s)", quoteJoin(expected))
			}

			return map[string]any{"error": msg}
		}

		rendered, err := template.Render(spec.Run, map[string]any{"args": merged})
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		return shellToolResult(ctx, rendered, env, outputLimit(spec.MaxOutputBytes))
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
// string value, in params order — so a custom tool reports every missing
// argument in one message instead of the model discovering them one failed
// render at a time.
func missingArgs(args map[string]any, params []string) []string {
	missing := make([]string, 0, len(params))

	for _, p := range params {
		if stringArg(args, p) == "" {
			missing = append(missing, p)
		}
	}

	return missing
}

// quoteJoin renders names as a comma-separated list of quoted names, e.g.
// `"a", "b"`.
func quoteJoin(names []string) string {
	quoted := make([]string, 0, len(names))

	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}

	return strings.Join(quoted, ", ")
}

func executeAgentTool(ctx context.Context, call *genai.FunctionCall, env toolEnv, registry map[string]toolImpl) map[string]any {
	impl, ok := registry[call.Name]
	if !ok {
		return map[string]any{"error": fmt.Sprintf("unknown tool %q", call.Name)}
	}

	return impl(ctx, call.Args, env)
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)

	return v
}

// intArg reads an integer tool argument. The genai/JSON path decodes a
// model-supplied number as float64; the int case exists only so
// Go-constructed args (tests, sub-agent/verdict plumbing) can pass a plain
// int without going through JSON first. The bool return distinguishes "not
// supplied" from "supplied as 0", which read_file's start_line/end_line
// both need (0 is not a valid line number, but its absence and its
// explicit-zero are different requests).
func intArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}
