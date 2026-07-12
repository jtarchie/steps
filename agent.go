package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// maxAgentTurns bounds one attempt's tool-calling loop. 3-6 round trips
// covers a typical review (list a dir, read a few files, run a command,
// respond); 8 leaves headroom while still bounding a runaway loop (a model
// that never stops requesting tools) to a small, predictable number of
// calls. It's a Go const, not a YAML field, because there's no evidence yet
// that pipelines need to tune it.
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

const agentSystemPromptTemplate = `You are an automated agent running as one step of a CI pipeline job. Your working directory is %s. Use the tools available to you (all scoped to that directory) to complete the task described below. When finished, reply with a final plain-text message and no further tool calls.`

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
// It returns the map sent back as the FunctionResponse — never a Go error;
// failures are reported to the model as {"error": ...} data so it can react
// on its next turn instead of aborting the whole attempt.
type toolImpl func(ctx context.Context, args map[string]any, dir string) map[string]any

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

// defaultAgentToolSpecs is used when a step's tools: list is empty —
// backward-compatible default of every built-in.
func defaultAgentToolSpecs() []ToolSpec {
	return []ToolSpec{{Builtin: "read_file"}, {Builtin: "list_dir"}, {Builtin: "run_shell"}}
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

func execReadFile(_ context.Context, args map[string]any, dir string) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		return map[string]any{"error": `read_file: missing required argument "path"`}
	}

	resolved, err := resolveAgentPath(dir, rel)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	data, err := os.ReadFile(resolved) //nolint:gosec // resolveAgentPath rejects paths escaping dir, the step's own workspace
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	return map[string]any{"content": truncateToolOutput(string(data))}
}

func execListDir(_ context.Context, args map[string]any, dir string) map[string]any {
	rel := stringArg(args, "path")
	if rel == "" {
		rel = "."
	}

	resolved, err := resolveAgentPath(dir, rel)
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

func execRunShell(ctx context.Context, args map[string]any, dir string) map[string]any {
	command := stringArg(args, "command")
	if command == "" {
		return map[string]any{"error": `run_shell: missing required argument "command"`}
	}

	return shellToolResult(ctx, command, dir)
}

// execCustomTool renders spec.Run against the model's args and shells it
// out. Model-supplied arg values are interpolated into the sh -c string, so
// a custom tool is a capability-curation convenience, not a hard sandbox —
// the same trust boundary as run_shell itself.
func execCustomTool(spec ToolSpec) toolImpl {
	return func(ctx context.Context, args map[string]any, dir string) map[string]any {
		rendered, err := Render(spec.Run, map[string]any{"args": args})
		if err != nil {
			return map[string]any{"error": err.Error()}
		}

		return shellToolResult(ctx, rendered, dir)
	}
}

func executeAgentTool(ctx context.Context, call *genai.FunctionCall, dir string, registry map[string]toolImpl) map[string]any {
	impl, ok := registry[call.Name]
	if !ok {
		return map[string]any{"error": fmt.Sprintf("unknown tool %q", call.Name)}
	}

	return impl(ctx, call.Args, dir)
}

// runAgentConversation runs one full attempt: an initial system+user
// message, then up to maxAgentTurns request/tool-execute/append round
// trips, terminating when the model responds with no tool calls. There is
// no turn-level checkpointing — a retry (see withRetry in runAgentStep)
// restarts the whole conversation from scratch. If a tool call already had
// a side effect (e.g. posting a PR review) before a later turn failed, a
// retry may re-invoke it again; pipeline prompts should tell the model to
// check current state before acting, the same caveat Concourse's own
// task.attempts carries for non-idempotent tasks.
func runAgentConversation(ctx context.Context, llm model.LLM, prompt, dir string, tools agentTools) (string, int, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: prompt}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: fmt.Sprintf(agentSystemPromptTemplate, dir)}}},
			Tools:             []*genai.Tool{tools.decls},
		},
	}

	for turn := range maxAgentTurns {
		resp, err := generateOnce(ctx, llm, req)
		if err != nil {
			return "", turn, err
		}

		req.Contents = append(req.Contents, resp.Content)

		calls, text := collectParts(resp.Content)
		if len(calls) == 0 {
			return text, turn + 1, nil
		}

		req.Contents = append(req.Contents, &genai.Content{
			Role:  genai.RoleUser,
			Parts: toolResponseParts(ctx, calls, dir, tools.registry),
		})
	}

	return "", maxAgentTurns, fmt.Errorf("agent exceeded %d turns without a final response", maxAgentTurns)
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

// toolResponseParts executes each requested tool and packages the results
// as FunctionResponse parts to feed back on the next turn.
func toolResponseParts(ctx context.Context, calls []*genai.FunctionCall, dir string, registry map[string]toolImpl) []*genai.Part {
	parts := make([]*genai.Part, 0, len(calls))

	for _, call := range calls {
		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: executeAgentTool(ctx, call, dir, registry)},
		})
	}

	return parts
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
// to step.Attempts times. It returns the hash to use as parentHash for the
// next step.
func runAgentStep(ctx context.Context, cfg *Config, jobName string, i int, step Step, workspaceDir string, store *Store, parentHash string) (string, error) {
	agent, err := cfg.FindAgent(step.Agent)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	baseURL, modelName, apiKeyEnv, requiresKey, err := resolveAgentTarget(agent.Source)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	dir, err := resolveAgentDir(workspaceDir, step.Dir)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	decls, registry, err := buildAgentTools(step.Tools)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	apiKey, err := lookupAPIKey(apiKeyEnv, requiresKey)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	content := agentNodeContent(step.Agent, step.Prompt, step.Dir, modelName, baseURL, step.Tools)

	hash, err := hashNode(NodeKindAgent, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "agent", "agent", step.Agent)

	fmt.Printf("agent: %s\n", step.Agent)

	node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindAgent, StepIndex: i, Resource: agent.Name, Content: content}
	llm := newAgentLLM(baseURL, modelName, apiKey)
	tools := agentTools{decls: decls, registry: registry}

	agentCtx, cancel := context.WithTimeout(ctx, agentStepTimeout)
	defer cancel()

	var (
		finalContent string
		turnsUsed    int
	)

	err = withRetry(agentCtx, step.Attempts, func(_ int) error {
		answer, turns, runErr := runAgentConversation(agentCtx, llm, step.Prompt, dir, tools)
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
