package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// maxAgentTurns is the default cap on one attempt's tool-calling loop when
// an agent doesn't set max_turns. 3-6 round trips covers a typical review
// (list a dir, read a few files, run a command, respond); 8 leaves headroom
// while still bounding a runaway loop (a model that never stops requesting
// tools) to a small, predictable number of calls.
const maxAgentTurns = 8

// agentStepTimeout bounds one agent step's total wall-clock time (across
// all attempts and turns). maxAgentTurns bounds the number of turns, but a
// single hung endpoint could otherwise block indefinitely — the OpenAI
// client sets no default request timeout and relies entirely on ctx.
const agentStepTimeout = 10 * time.Minute

// maxToolOutputBytes caps how much of a tool's textual output (file
// contents, command stdout/stderr) is handed back to the model. A runaway
// command (cat a huge file, find /) would otherwise buffer megabytes into
// memory and flood the model's context window (cost, and possible
// context-limit failures). Anything beyond this is truncated with a
// trailing marker so the model knows output was cut.
const maxToolOutputBytes = 100_000

// defaultAgentPersona is the system persona used when an agent doesn't set
// its own `system:`.
const defaultAgentPersona = `You are an automated agent running as one step of a CI pipeline job.`

// agentOperatingNote is appended to the persona to give the model its
// operating context (working directory + tool discipline). Filled with the
// resolved working directory.
const agentOperatingNote = `Your working directory is %s. Use the tools available to you (all scoped to that directory) to complete the task described below. When finished, reply with a final plain-text message and no further tool calls.`

// buildSystemMessage combines an agent's persona with the operating note for
// a given working directory.
func buildSystemMessage(persona, dir string) string {
	if persona == "" {
		persona = defaultAgentPersona
	}

	return persona + "\n\n" + fmt.Sprintf(agentOperatingNote, dir)
}

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
}

// resolveAgentTarget interprets an optional "provider/" prefix on
// source.Model (e.g. "openrouter/anthropic/claude-3.5-sonnet") against
// agentProviders, splitting on the first "/" so a provider's own slashed
// model IDs survive intact. source.Endpoint/APIKeyEnv, when set, always
// override the derived values. A model with no recognized provider prefix
// requires an explicit source.Endpoint.
func resolveAgentTarget(source AgentSource) (baseURL, modelName, apiKeyEnv string, requiresKey bool, err error) {
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

		return ensureTrailingSlash(baseURL), rest, apiKeyEnv, provider.requiresKey || source.APIKeyEnv != "", nil
	}

	if source.Endpoint == "" {
		return "", "", "", false, fmt.Errorf("model %q has no known provider prefix; set source.endpoint", source.Model)
	}

	return ensureTrailingSlash(source.Endpoint), source.Model, source.APIKeyEnv, source.APIKeyEnv != "", nil
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

// lookupAPIKey reads the API key from the OS environment variable named by
// envVar. When required, a missing/empty variable (or a missing envVar
// name) is a hard error — sending a blank Authorization header would just
// produce a confusing 401 from the endpoint. When not required (local
// providers with no default key), a missing key resolves to "" and no
// Authorization header is sent.
func lookupAPIKey(envVar string, required bool) (string, error) {
	if envVar == "" {
		if required {
			return "", errors.New("agent source is missing api_key_env")
		}

		return "", nil
	}

	val, ok := os.LookupEnv(envVar)
	if !ok || val == "" {
		if required {
			return "", fmt.Errorf("environment variable %q (api_key_env) is not set", envVar)
		}

		return "", nil
	}

	return val, nil
}

// newAgentLLM constructs the model.LLM used for real runs, given the
// already-resolved base URL/model/key (see resolveAgentTarget). Returning
// the model.LLM interface (not the concrete *openai.Model) keeps
// runAgentConversation testable against an in-process fake.
func newAgentLLM(baseURL, modelName, apiKey string) model.LLM {
	return genaiopenai.New(genaiopenai.Config{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		ModelName: modelName,
	})
}

// toolImpl executes one resolved tool against dir, given the model's args.
// It returns the map sent back as the FunctionResponse, plus an error. Most
// failures are reported to the model as {"error": ...} data (err is nil) so
// it can react on its next turn instead of aborting the whole attempt; a
// non-nil err means the failure is fatal to the whole agent step — currently
// only a required: true custom tool's nonzero exit does this, mirroring how
// a put step's failure always aborts the job rather than being handed to
// anything for a decision.
type toolImpl func(ctx context.Context, args map[string]any, dir string) (map[string]any, error)

// agentTools bundles what buildAgentTools produced: the declarations sent
// to the model and the registry used to execute the calls it makes.
type agentTools struct {
	decls    *genai.Tool
	registry map[string]toolImpl
}

type builtinTool struct {
	decl *genai.FunctionDeclaration
	impl toolImpl
}

func builtinAgentTools() map[string]builtinTool {
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
				Description: "Run a shell command via `sh -c`, with cwd set to the step's working directory. Returns stdout, stderr, and exit_code.",
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

// defaultAgentToolSpecs is used when an agent grants no tools — the default
// is every built-in.
func defaultAgentToolSpecs() []ToolSpec {
	return []ToolSpec{{Builtin: "read_file"}, {Builtin: "list_dir"}, {Builtin: "run_shell"}}
}

// toolSpecName is the name a ToolSpec is referenced by: the builtin name for
// a builtin, or the custom tool's name.
func toolSpecName(spec ToolSpec) string {
	if spec.Builtin != "" {
		return spec.Builtin
	}

	return spec.Name
}

// grantedToolIndex maps each tool an agent grants (by reference name) to its
// spec. An agent that grants nothing is treated as granting every built-in,
// so the simple "no tools: block" case still works.
func grantedToolIndex(agentTools []ToolSpec) map[string]ToolSpec {
	specs := agentTools
	if len(specs) == 0 {
		specs = defaultAgentToolSpecs()
	}

	index := make(map[string]ToolSpec, len(specs))
	for _, spec := range specs {
		index[toolSpecName(spec)] = spec
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

		effective = append(effective, spec)
	}

	return effective, nil
}

// buildAgentTools turns a step's resolved tools: list into the genai
// declarations sent to the model and a name -> toolImpl execution registry.
// An empty specs enables every built-in. A duplicate tool name (built-in vs
// custom, or two customs) is an error, so the model never sees an
// ambiguous function set.
func buildAgentTools(specs []ToolSpec) (*genai.Tool, map[string]toolImpl, error) {
	if len(specs) == 0 {
		specs = defaultAgentToolSpecs()
	}

	builtins := builtinAgentTools()
	decls := make([]*genai.FunctionDeclaration, 0, len(specs))
	registry := make(map[string]toolImpl, len(specs))

	for _, spec := range specs {
		decl, impl, err := resolveToolSpec(spec, builtins)
		if err != nil {
			return nil, nil, err
		}

		if _, exists := registry[decl.Name]; exists {
			return nil, nil, fmt.Errorf("duplicate tool name %q", decl.Name)
		}

		decls = append(decls, decl)
		registry[decl.Name] = impl
	}

	return &genai.Tool{FunctionDeclarations: decls}, registry, nil
}

func resolveToolSpec(spec ToolSpec, builtins map[string]builtinTool) (*genai.FunctionDeclaration, toolImpl, error) {
	if spec.Builtin != "" {
		bt, ok := builtins[spec.Builtin]
		if !ok {
			return nil, nil, fmt.Errorf("unknown builtin tool %q", spec.Builtin)
		}

		return bt.decl, bt.impl, nil
	}

	if spec.Name == "" || spec.Run == "" {
		return nil, nil, errors.New("agent tool: custom tool requires both name and run")
	}

	params := inferToolParams(spec.Run)
	properties := make(map[string]*genai.Schema, len(params))

	for _, p := range params {
		properties[p] = &genai.Schema{Type: genai.TypeString}
	}

	decl := &genai.FunctionDeclaration{
		Name:        spec.Name,
		Description: spec.Description,
		Parameters:  &genai.Schema{Type: genai.TypeObject, Properties: properties, Required: params},
	}

	return decl, execCustomTool(spec), nil
}

//nolint:gochecknoglobals // compiled once, read-only
var agentToolArgPattern = regexp.MustCompile(`\{\{-?\s*\.args\.([A-Za-z_]\w*)\s*-?\}\}`)

// inferToolParams scans a custom tool's run template for {{ .args.NAME }}
// references, returning each distinct NAME once, in first-seen order.
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
// rejects any result that escapes dir, so a crafted "../../etc/passwd"
// style path can't read outside the step's working directory.
func resolveAgentPath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative", rel)
	}

	resolved := filepath.Clean(filepath.Join(dir, rel))
	base := filepath.Clean(dir)

	if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the working directory", rel)
	}

	return resolved, nil
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

// maxErrorDetailBytes caps how much of a failed required tool's stderr gets
// embedded in the Go error returned up through withRetry, job failure, and
// the top-level log — a much tighter cap than maxToolOutputBytes, since that
// text (unlike a tool's response to the model) ends up in a single log line
// rather than the model's own context.
const maxErrorDetailBytes = 2_000

// errorDetail returns the tail of s (trimmed), capped at maxErrorDetailBytes
// — the end of stderr is usually where the actual failure is reported, so
// keeping the tail rather than the head favors the most useful part.
func errorDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxErrorDetailBytes {
		return s
	}

	return "...(truncated)... " + s[len(s)-maxErrorDetailBytes:]
}

// shellToolResult builds the FunctionResponse map for a shell-backed tool
// (run_shell and every custom tool), truncating the captured streams.
func shellToolResult(ctx context.Context, command, dir string) map[string]any {
	stdout, stderr, exitCode, err := RunShellCaptureFull(ctx, command, dir)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{
		"stdout":    truncateToolOutput(stdout),
		"stderr":    truncateToolOutput(stderr),
		"exit_code": exitCode,
	}
}

func execReadFile(_ context.Context, args map[string]any, dir string) (map[string]any, error) {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `read_file: missing required argument "path"`}, nil
	}

	resolved, err := resolveAgentPath(dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}, nil //nolint:nilerr // read_file failures are always reported as data, never fatal
	}

	data, err := os.ReadFile(resolved) //nolint:gosec // resolveAgentPath rejects paths escaping dir, the step's own workspace
	if err != nil {
		return map[string]any{"error": err.Error()}, nil //nolint:nilerr // read_file failures are always reported as data, never fatal
	}

	return map[string]any{"content": truncateToolOutput(string(data))}, nil
}

func execListDir(_ context.Context, args map[string]any, dir string) (map[string]any, error) {
	rel := stringArg(args, "path")
	if rel == "" {
		rel = "."
	}

	resolved, err := resolveAgentPath(dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}, nil //nolint:nilerr // list_dir failures are always reported as data, never fatal
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return map[string]any{"error": err.Error()}, nil //nolint:nilerr // list_dir failures are always reported as data, never fatal
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

	return map[string]any{"entries": items}, nil
}

func execRunShell(ctx context.Context, args map[string]any, dir string) (map[string]any, error) {
	command := stringArg(args, "command")
	if command == "" {
		return map[string]any{"error": `run_shell: missing required argument "command"`}, nil
	}

	return shellToolResult(ctx, command, dir), nil
}

// execCustomTool renders spec.Run against the model's args and shells it
// out. Model-supplied arg values are interpolated into the sh -c string, so
// a custom tool is a capability-curation convenience, not a hard sandbox —
// the same trust boundary as run_shell itself. When spec.Required is set, a
// nonzero exit — or the command failing to run at all — is returned as a Go
// error (fatal to the step) rather than just data left for the model to
// interpret.
func execCustomTool(spec ToolSpec) toolImpl {
	return func(ctx context.Context, args map[string]any, dir string) (map[string]any, error) {
		rendered, err := Render(spec.Run, map[string]any{"args": args})
		if err != nil {
			return map[string]any{"error": err.Error()}, nil //nolint:nilerr // a bad template is a data error the model sees, not a fatal one — required: only gates the command's own execution
		}

		result := shellToolResult(ctx, rendered, dir)

		if spec.Required {
			// shellToolResult reports a command that never ran at all (e.g. a
			// missing working directory) as {"error": ...} with no exit_code
			// key — that must be just as fatal as a nonzero exit, or a
			// required tool that fails to run is silently treated as
			// optional.
			if errMsg, ok := result["error"].(string); ok {
				return result, fmt.Errorf("required tool %q failed to run: %s", spec.Name, errorDetail(errMsg))
			}

			if exitCode, ok := result["exit_code"].(int); ok && exitCode != 0 {
				return result, fmt.Errorf("required tool %q exited %d: %s", spec.Name, exitCode, errorDetail(fmt.Sprint(result["stderr"])))
			}
		}

		return result, nil
	}
}

func executeAgentTool(ctx context.Context, call *genai.FunctionCall, dir string, registry map[string]toolImpl) (map[string]any, error) {
	impl, ok := registry[call.Name]
	if !ok {
		return map[string]any{"error": fmt.Sprintf("unknown tool %q", call.Name)}, nil
	}

	return impl(ctx, call.Args, dir)
}

// agentGenParams holds the generation dials an agent configures. Unset
// fields (nil pointers, zero maxTokens, empty reasoning) are left off the
// request so the model's own defaults apply.
type agentGenParams struct {
	temperature *float64
	topP        *float64
	maxTokens   int
	reasoning   string // "", "low", "medium", or "high"
}

//nolint:gochecknoglobals // static, read-only lookup table
var reasoningLevels = map[string]genai.ThinkingLevel{
	"low":    genai.ThinkingLevelLow,
	"medium": genai.ThinkingLevelMedium,
	"high":   genai.ThinkingLevelHigh,
}

// applyTo sets the configured dials on a genai generation config.
func (p agentGenParams) applyTo(cfg *genai.GenerateContentConfig) {
	if p.temperature != nil {
		t := float32(*p.temperature)
		cfg.Temperature = &t
	}

	if p.topP != nil {
		t := float32(*p.topP)
		cfg.TopP = &t
	}

	if p.maxTokens > 0 {
		tokens := min(p.maxTokens, math.MaxInt32)
		cfg.MaxOutputTokens = int32(tokens) //nolint:gosec // clamped to MaxInt32 on the line above
	}

	if level, ok := reasoningLevels[p.reasoning]; ok {
		cfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: level}
	}
}

// resolvedInvocation is an agent + step reduced to everything needed to hash
// and run the step: the resolved connection, persona, dials, limits, and the
// effective (merged) tool set. resolveAgentInvocation produces it for both
// planning (merkle.go's agentNode) and execution (runAgentStep), so both
// compute identical hashes.
type resolvedInvocation struct {
	agentName   string
	baseURL     string
	modelName   string
	apiKeyEnv   string
	requiresKey bool
	persona     string
	genParams   agentGenParams
	maxTurns    int
	attempts    int
	toolSpecs   []ToolSpec
}

// resolveAgentInvocation resolves the agent named by step against cfg,
// applying provider-prefix resolution, tool-grant merging, and defaulting
// (step.Attempts defaults to 1 — retries are a per-task concern, not part of
// the agent's config; agent.MaxTurns defaults to maxAgentTurns).
func resolveAgentInvocation(cfg *Config, step Step) (resolvedInvocation, error) {
	agent, err := cfg.FindAgent(step.Agent)
	if err != nil {
		return resolvedInvocation{}, err
	}

	baseURL, modelName, apiKeyEnv, requiresKey, err := resolveAgentTarget(agent.Source)
	if err != nil {
		return resolvedInvocation{}, err
	}

	toolSpecs, err := resolveEffectiveTools(agent.Tools, step.Tools)
	if err != nil {
		return resolvedInvocation{}, err
	}

	reasoning := strings.ToLower(agent.ReasoningEffort)
	if reasoning != "" {
		if _, ok := reasoningLevels[reasoning]; !ok {
			return resolvedInvocation{}, fmt.Errorf("reasoning_effort %q must be one of low, medium, high", agent.ReasoningEffort)
		}
	}

	maxTurns := agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = maxAgentTurns
	}

	attempts := step.Attempts
	if attempts <= 0 {
		attempts = 1
	}

	return resolvedInvocation{
		agentName:   agent.Name,
		baseURL:     baseURL,
		modelName:   modelName,
		apiKeyEnv:   apiKeyEnv,
		requiresKey: requiresKey,
		persona:     agent.System,
		genParams: agentGenParams{
			temperature: agent.Temperature,
			topP:        agent.TopP,
			maxTokens:   agent.MaxTokens,
			reasoning:   reasoning,
		},
		maxTurns:  maxTurns,
		attempts:  attempts,
		toolSpecs: toolSpecs,
	}, nil
}

// agentContentMap is the content hashed for an agent node: everything that
// determines the model's output (agent, prompt, dir, resolved model/endpoint,
// persona, dials, and the effective tool set). Attempts is excluded (a pure
// retry policy doesn't change the intended result); the API key and its env
// var name are excluded (nothing secret-adjacent belongs in hashed content).
func agentContentMap(step Step, ri resolvedInvocation) map[string]any {
	toolsContent := make([]map[string]any, len(ri.toolSpecs))
	for i, t := range ri.toolSpecs {
		toolsContent[i] = map[string]any{
			"builtin":     t.Builtin,
			"name":        t.Name,
			"description": t.Description,
			"run":         t.Run,
		}
	}

	return map[string]any{
		"agent":            step.Agent,
		"prompt":           step.Prompt,
		"dir":              step.Dir,
		"model":            ri.modelName,
		"endpoint":         ri.baseURL,
		"system":           ri.persona,
		"temperature":      ri.genParams.temperature,
		"top_p":            ri.genParams.topP,
		"max_tokens":       ri.genParams.maxTokens,
		"reasoning_effort": ri.genParams.reasoning,
		"max_turns":        ri.maxTurns,
		"tools":            toolsContent,
	}
}

// agentConversation is one runnable attempt's inputs.
type agentConversation struct {
	system   string
	prompt   string
	dir      string
	tools    agentTools
	params   agentGenParams
	maxTurns int
}

// buildAgentRequest builds a fresh LLM request (system + user prompt + tools
// + dials). A fresh one is built per attempt so a retry starts from a clean
// conversation rather than the grown Contents of a failed attempt.
func buildAgentRequest(conv agentConversation) *model.LLMRequest {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: conv.system}}},
		Tools:             []*genai.Tool{conv.tools.decls},
	}
	conv.params.applyTo(cfg)

	return &model.LLMRequest{
		Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: conv.prompt}}}},
		Config:   cfg,
	}
}

// runAgentConversation runs one full attempt: an initial system+user
// message, then up to conv.maxTurns request/tool-execute/append round trips,
// terminating when the model responds with no tool calls. There is no
// turn-level checkpointing — a retry (see withRetry in runAgentStep) restarts
// the whole conversation from scratch. If a tool call already had a side
// effect (e.g. posting a PR review) before a later turn failed, a retry may
// re-invoke it again; pipeline prompts should tell the model to check current
// state before acting, the same caveat Concourse's own task.attempts carries
// for non-idempotent tasks.
func runAgentConversation(ctx context.Context, llm model.LLM, conv agentConversation) (string, int, error) {
	req := buildAgentRequest(conv)

	for turn := range conv.maxTurns {
		resp, err := generateOnce(ctx, llm, req)
		if err != nil {
			return "", turn, err
		}

		req.Contents = append(req.Contents, resp.Content)

		calls, text := collectParts(resp.Content)
		if len(calls) == 0 {
			return text, turn + 1, nil
		}

		parts, toolErr := toolResponseParts(ctx, calls, conv.dir, conv.tools.registry)
		if toolErr != nil {
			return "", turn + 1, toolErr
		}

		req.Contents = append(req.Contents, &genai.Content{
			Role:  genai.RoleUser,
			Parts: parts,
		})
	}

	return "", conv.maxTurns, fmt.Errorf("agent exceeded %d turns without a final response", conv.maxTurns)
}

// defaultFixPrompt is used when a task's fix: supplies no prompt of its own.
// %q is the task name (also the name of the injected rerun tool). The
// captured failure output is appended after this.
const defaultFixPrompt = `A command that must pass has just failed; its output is below. Investigate the working directory, make the smallest change that resolves the failure, then call the %q tool to re-run the command and confirm it passes. Repeat until it passes, then reply with a brief summary and stop.`

// runFixAgent invokes a failed task's fix: agent. It reuses the normal
// agent-invocation resolution (tool grant, dials, attempts, max_turns) by
// projecting the FixSpec onto an agent Step, then injects the parent task as
// a zero-arg rerun tool — the task's own run: command (never its fix:, so a
// rerun can't recurse), exposed under the task's name — and seeds the
// conversation with the captured failure output. It does no merkle/store
// recording: the enclosing task step records the overall outcome, and the
// task's re-run (not the model's word) is the verdict.
func runFixAgent(ctx context.Context, cfg *Config, task Step, failureOutput, workspaceDir string) error {
	fix := task.Fix

	// Project the fix spec onto an agent Step so resolveAgentInvocation can
	// resolve grants/dials/limits exactly as it does for a real agent step.
	ri, err := resolveAgentInvocation(cfg, Step{
		Agent:    fix.Agent,
		Prompt:   fix.Prompt,
		Dir:      fix.Dir,
		Tools:    fix.Tools,
		Attempts: fix.Attempts,
	})
	if err != nil {
		return err
	}

	dir, err := resolveAgentDir(workspaceDir, fix.Dir)
	if err != nil {
		return err
	}

	// Expand "no tools granted means all built-ins" before appending, so the
	// injected task tool doesn't accidentally suppress the default built-ins.
	baseTools := ri.toolSpecs
	if len(baseTools) == 0 {
		baseTools = defaultAgentToolSpecs()
	}

	taskTool := ToolSpec{
		Name:        task.Task,
		Description: fmt.Sprintf("Re-run the %q task's command. Returns exit_code, stdout, stderr.", task.Task),
		Run:         task.Run,
	}
	toolSpecs := append(append([]ToolSpec{}, baseTools...), taskTool)

	decls, registry, err := buildAgentTools(toolSpecs)
	if err != nil {
		return err
	}

	apiKey, err := lookupAPIKey(ri.apiKeyEnv, ri.requiresKey)
	if err != nil {
		return err
	}

	prompt := fix.Prompt
	if prompt == "" {
		prompt = fmt.Sprintf(defaultFixPrompt, task.Task)
	}

	prompt += "\n\n--- failure output ---\n" + truncateToolOutput(failureOutput)

	conv := agentConversation{
		system:   buildSystemMessage(ri.persona, dir),
		prompt:   prompt,
		dir:      dir,
		tools:    agentTools{decls: decls, registry: registry},
		params:   ri.genParams,
		maxTurns: ri.maxTurns,
	}
	llm := newAgentLLM(ri.baseURL, ri.modelName, apiKey)

	agentCtx, cancel := context.WithTimeout(ctx, agentStepTimeout)
	defer cancel()

	return withRetry(agentCtx, ri.attempts, func(_ int) error {
		_, _, runErr := runAgentConversation(agentCtx, llm, conv)

		return runErr
	})
}

// generateOnce drains llm.GenerateContent's iterator for the non-streaming
// case, which yields exactly one (response, error) pair.
func generateOnce(ctx context.Context, llm model.LLM, req *model.LLMRequest) (*model.LLMResponse, error) {
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return nil, fmt.Errorf("agent: generate content: %w", err)
		}

		if resp == nil || resp.Content == nil {
			return nil, errors.New("agent: model returned an empty response")
		}

		return resp, nil
	}

	return nil, errors.New("agent: model returned no response")
}

// collectParts splits a model turn into the tool calls it requested and the
// plain (non-thought) text it emitted.
func collectParts(content *genai.Content) (calls []*genai.FunctionCall, text string) {
	var b strings.Builder

	for _, part := range content.Parts {
		switch {
		case part.FunctionCall != nil:
			calls = append(calls, part.FunctionCall)
		case part.Text != "" && !part.Thought:
			b.WriteString(part.Text)
		}
	}

	return calls, b.String()
}

// toolResponseParts executes each requested tool and packages the results as
// FunctionResponse parts to feed back on the next turn. Every call in the
// turn still runs (a fatal tool call doesn't short-circuit its siblings);
// every fatal error encountered is joined and returned alongside the parts,
// so the caller can abort the conversation with the full picture instead of
// just the first of several required tools that failed in the same turn.
func toolResponseParts(ctx context.Context, calls []*genai.FunctionCall, dir string, registry map[string]toolImpl) ([]*genai.Part, error) {
	parts := make([]*genai.Part, 0, len(calls))

	var errs []error

	for _, call := range calls {
		response, err := executeAgentTool(ctx, call, dir, registry)
		if err != nil {
			errs = append(errs, err)
		}

		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: response},
		})
	}

	return parts, errors.Join(errs...)
}

// resolveAgentDir joins and validates a step's working directory.
func resolveAgentDir(workspaceDir, stepDir string) (string, error) {
	dir := workspaceDir
	if stepDir != "" {
		dir = filepath.Join(workspaceDir, stepDir)
	}

	_, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("working directory %q: %w", dir, err)
	}

	return dir, nil
}

// recordAgentFailure records a failed agent step the same way
// runTaskStep/runPutStep do — best-effort, errors ignored, since a failure
// to record must not mask the original error being returned to the caller.
func recordAgentFailure(ctx context.Context, store *Store, node Node, jobName string, runErr error) {
	_ = store.RecordNode(ctx, node, jobName, "failed", nil, runErr)
	_ = store.RecordJobRun(ctx, jobName, node.Hash, "failed", runErr)
}

// runAgentStep hashes step against parentHash (agent steps are never
// skippable — see runSteps) and runs it, retrying the whole conversation up
// to the resolved attempt count. It returns the hash to use as parentHash
// for the next step.
func runAgentStep(ctx context.Context, cfg *Config, jobName string, i int, step Step, workspaceDir string, store *Store, parentHash string) (string, error) {
	ri, err := resolveAgentInvocation(cfg, step)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	dir, err := resolveAgentDir(workspaceDir, step.Dir)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	decls, registry, err := buildAgentTools(ri.toolSpecs)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	apiKey, err := lookupAPIKey(ri.apiKeyEnv, ri.requiresKey)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	content := agentContentMap(step, ri)

	hash, err := hashNode(NodeKindAgent, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "agent", "agent", step.Agent)

	fmt.Printf("agent: %s\n", step.Agent)

	node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindAgent, StepIndex: i, Resource: ri.agentName, Content: content}

	conv := agentConversation{
		system:   buildSystemMessage(ri.persona, dir),
		prompt:   step.Prompt,
		dir:      dir,
		tools:    agentTools{decls: decls, registry: registry},
		params:   ri.genParams,
		maxTurns: ri.maxTurns,
	}
	llm := newAgentLLM(ri.baseURL, ri.modelName, apiKey)

	agentCtx, cancel := context.WithTimeout(ctx, agentStepTimeout)
	defer cancel()

	var (
		finalContent string
		turnsUsed    int
	)

	err = withRetry(agentCtx, ri.attempts, func(_ int) error {
		answer, turns, runErr := runAgentConversation(agentCtx, llm, conv)
		turnsUsed = turns

		if runErr != nil {
			return runErr
		}

		finalContent = answer

		return nil
	})
	if err != nil {
		recordAgentFailure(ctx, store, node, jobName, err)

		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	result := map[string]any{"response": finalContent, "turns": turnsUsed}

	err = store.RecordNode(ctx, node, jobName, "succeeded", result, nil)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return hash, nil
}
