package agent

import (
	"context"
	"errors"
	"iter"
	"slices"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/shell"
)

// testMaxTurns bounds the tool-calling loop in these tests. It's a local
// constant rather than config's defaultMaxAgentTurns (unexported in
// internal/config) since these tests only need some fixed upper bound, not
// the actual default agents resolve to.
const testMaxTurns = 8

// fakeLLM is an in-process model.LLM stand-in: responses[i] (or errs[i], if
// set) is returned for the i-th call. It records every request it receives
// so tests can assert on conversation-thread contents.
type fakeLLM struct {
	responses []*model.LLMResponse
	errs      []error
	calls     int
	requests  []*model.LLMRequest
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		i := f.calls
		f.calls++
		// runAgentConversation reuses and mutates the same *LLMRequest across
		// turns (e.g. Config.ToolConfig is set then cleared) — record a
		// shallow copy of req and req.Config so a later turn's mutation
		// doesn't retroactively change what an earlier recorded request
		// looked like at call time.
		snapshot := *req
		if req.Config != nil {
			cfg := *req.Config
			snapshot.Config = &cfg
		}

		f.requests = append(f.requests, &snapshot)

		if i < len(f.errs) && f.errs[i] != nil {
			yield(nil, f.errs[i])

			return
		}

		if i < len(f.responses) {
			yield(f.responses[i], nil)

			return
		}

		yield(nil, errors.New("fakeLLM: no more responses configured"))
	}
}

func TestRunAgentConversationMultiTurnToolCalling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "run_shell", Args: map[string]any{"command": "echo hi"}}},
				},
			}},
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "done"}},
			}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "do the thing", dir))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.text != "done" {
		t.Errorf("content = %q, want %q", res.text, "done")
	}

	if res.turns != 2 {
		t.Errorf("turns = %d, want 2", res.turns)
	}

	if len(fake.requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(fake.requests))
	}

	if !hasFunctionResponseNamed(fake.requests[1].Contents, "run_shell") {
		t.Error("expected the second request to include a FunctionResponse for run_shell")
	}
}

// newTestConversation builds an agentConversation with all built-in tools
// for exercising the loop against a fakeLLM.
func newTestConversation(t *testing.T, prompt, dir string) agentConversation {
	t.Helper()

	decls, registry, _, err := buildAgentTools(context.Background(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner("", dir)
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		prompt:   prompt,
		env:      toolEnv{dir: dir, runner: runner},
		tools:    agentTools{decls: decls, registry: registry},
		maxTurns: testMaxTurns,
	}
}

// hasFunctionResponseNamed reports whether any part across contents is a
// FunctionResponse for the named tool.
func hasFunctionResponseNamed(contents []*genai.Content, name string) bool {
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}

	return false
}

func TestRunAgentConversationExceedsMaxTurns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	toolCallResp := &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "run_shell", Args: map[string]any{"command": "true"}}}},
		},
	}

	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = toolCallResp
	}

	fake := &fakeLLM{responses: responses}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "loop forever", dir))
	if err == nil {
		t.Fatal("expected an error when the model never stops calling tools")
	}

	if res.turns != testMaxTurns {
		t.Errorf("turns = %d, want %d", res.turns, testMaxTurns)
	}
}

// requiredToolConversation builds an agentConversation with a single
// required: true custom tool ("post_review", always succeeds if called), so
// tests can drive a fakeLLM that either calls it or skips straight to a
// final answer.
func requiredToolConversation(t *testing.T, dir string) agentConversation {
	t.Helper()

	specs := []config.ToolSpec{{Name: "post_review", Run: "true", Required: true}}

	decls, registry, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner("", dir)
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		prompt:   "review it",
		env:      toolEnv{dir: dir, runner: runner},
		tools:    agentTools{decls: decls, registry: registry, required: requiredToolNames(specs)},
		maxTurns: testMaxTurns,
	}
}

// TestRunAgentConversationForcesRequiredToolCall drives a model that tries
// to stop without calling the required tool, then (once forced) complies —
// simulating a provider that honors tool_choice, which real OpenAI-compatible
// backends do (see genaiopenai's ToolConfig mapping). It also asserts the
// forcing request itself was actually sent, not just that things worked out.
func TestRunAgentConversationForcesRequiredToolCall(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "looks fine, done"}}}},
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "post_review", Args: map[string]any{}}}},
			}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "posted"}}}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, requiredToolConversation(t, t.TempDir()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.text != "posted" {
		t.Errorf("content = %q, want %q", res.text, "posted")
	}

	if len(fake.requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(fake.requests))
	}

	forcedCfg := fake.requests[1].Config.ToolConfig
	if forcedCfg == nil || forcedCfg.FunctionCallingConfig == nil {
		t.Fatal("expected the second request to force a tool call via ToolConfig")
	}

	fcc := forcedCfg.FunctionCallingConfig
	if fcc.Mode != genai.FunctionCallingConfigModeAny || !slices.Contains(fcc.AllowedFunctionNames, "post_review") {
		t.Errorf("expected ToolConfig to force post_review specifically, got mode=%v names=%v", fcc.Mode, fcc.AllowedFunctionNames)
	}
}

// TestRunAgentConversationForcesRequiredToolCallStringOnly covers the
// toolChoiceStringOnly fallback (config.ResolvedInvocation.StringOnlyToolChoice)
// for providers whose OpenAI-compat server rejects the named-function
// tool_choice object (e.g. LM Studio) — genaiopenai maps an empty
// AllowedFunctionNames under ModeAny to the generic string tool_choice:
// "required" instead.
func TestRunAgentConversationForcesRequiredToolCallStringOnly(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "looks fine, done"}}}},
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "post_review", Args: map[string]any{}}}},
			}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "posted"}}}},
		},
	}

	conv := requiredToolConversation(t, t.TempDir())
	conv.toolChoiceStringOnly = true

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.text != "posted" {
		t.Errorf("content = %q, want %q", res.text, "posted")
	}

	if len(fake.requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(fake.requests))
	}

	forcedCfg := fake.requests[1].Config.ToolConfig
	if forcedCfg == nil || forcedCfg.FunctionCallingConfig == nil {
		t.Fatal("expected the second request to force a tool call via ToolConfig")
	}

	fcc := forcedCfg.FunctionCallingConfig
	if fcc.Mode != genai.FunctionCallingConfigModeAny || len(fcc.AllowedFunctionNames) != 0 {
		t.Errorf("expected ToolConfig to force any tool call generically (no AllowedFunctionNames), got mode=%v names=%v", fcc.Mode, fcc.AllowedFunctionNames)
	}
}

// TestRunAgentConversationRequiredToolNeverCalled covers the safety bound: a
// model that keeps trying to stop without calling the required tool even
// after being forced (our fakeLLM doesn't actually honor tool_choice, unlike
// a real provider) must still hit maxTurns rather than loop forever.
func TestRunAgentConversationRequiredToolNeverCalled(t *testing.T) {
	t.Parallel()

	stopResp := &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "looks fine, done"}}},
	}

	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = stopResp
	}

	fake := &fakeLLM{responses: responses}

	res, err := runAgentConversation(context.Background(), fake, requiredToolConversation(t, t.TempDir()))
	if err == nil {
		t.Fatal("expected an error: the required tool was never called even after being forced")
	}

	if res.turns != testMaxTurns {
		t.Errorf("turns = %d, want %d", res.turns, testMaxTurns)
	}
}

func TestRunAgentConversationRequiredToolCalled(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "post_review", Args: map[string]any{}}}},
			}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "posted"}}}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, requiredToolConversation(t, t.TempDir()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.text != "posted" {
		t.Errorf("content = %q, want %q", res.text, "posted")
	}
}

// TestRunAgentConversationRecoversFailedRequiredTool is the core behavior
// this design exists for: a required tool that fails once and succeeds on a
// second call recovers entirely within one conversation — no retry.Do cold
// restart (that would rebuild the request from the original prompt with
// empty history; this test only ever calls runAgentConversation once, so a
// restart isn't even available — recovery has to happen in-session or not
// at all). It also confirms the model actually saw the first failure before
// trying again, not just that things happened to work out.
func TestRunAgentConversationRecoversFailedRequiredTool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Fails on its first invocation, succeeds on every one after — a marker
	// file tracks which call this is.
	specs := []config.ToolSpec{{
		Name:     "post_review",
		Run:      "[ -f done ] && exit 0 || { touch done; exit 1; }",
		Required: true,
	}}

	decls, registry, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner("", dir)
	if err != nil {
		t.Fatal(err)
	}

	conv := agentConversation{
		prompt:   "review it",
		env:      toolEnv{dir: dir, runner: runner},
		tools:    agentTools{decls: decls, registry: registry, required: requiredToolNames(specs)},
		maxTurns: testMaxTurns,
	}

	call := &genai.Part{FunctionCall: &genai.FunctionCall{ID: "call", Name: "post_review", Args: map[string]any{}}}

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{call}}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{call}}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "posted"}}}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.text != "posted" {
		t.Errorf("content = %q, want %q", res.text, "posted")
	}

	if len(fake.requests) != 3 {
		t.Fatalf("expected exactly 3 requests (one conversation, no restart), got %d", len(fake.requests))
	}

	exitCode, ok := functionResponseExitCode(fake.requests[1].Contents, "post_review")
	if !ok || exitCode != 1 {
		t.Errorf("expected the second request to carry the first call's exit_code 1, got %v (ok=%v)", exitCode, ok)
	}
}

// functionResponseExitCode returns the exit_code from the last FunctionResponse
// named name across contents, and whether one was found.
func functionResponseExitCode(contents []*genai.Content, name string) (int, bool) {
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				code, ok := p.FunctionResponse.Response["exit_code"].(int)

				return code, ok
			}
		}
	}

	return 0, false
}

func TestWithRetryRetriesAgentConversationOnFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	conv := newTestConversation(t, "hi", dir)

	fake := &fakeLLM{
		errs: []error{errors.New("transient failure")},
		responses: []*model.LLMResponse{
			nil,
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}}},
		},
	}

	var finalContent string

	err := retry.Do(context.Background(), 3, func(_ int) error {
		res, runErr := runAgentConversation(context.Background(), fake, conv)
		if runErr != nil {
			return runErr
		}

		finalContent = res.text

		return nil
	})
	if err != nil {
		t.Fatalf("retry.Do: %v", err)
	}

	if finalContent != "ok" {
		t.Errorf("finalContent = %q, want %q", finalContent, "ok")
	}

	if fake.calls != 2 {
		t.Errorf("fake.calls = %d, want 2", fake.calls)
	}
}

func TestApplyGenParams(t *testing.T) {
	t.Parallel()

	temp := 0.2
	topP := 0.9
	cfg := &genai.GenerateContentConfig{}
	agentGenParams{temperature: &temp, topP: &topP, maxTokens: 512, reasoning: "high"}.applyTo(cfg)

	if got := floatPtrVal(cfg.Temperature); got != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got)
	}

	if got := floatPtrVal(cfg.TopP); got != 0.9 {
		t.Errorf("top_p = %v, want 0.9", got)
	}

	if cfg.MaxOutputTokens != 512 {
		t.Errorf("max_output_tokens = %d, want 512", cfg.MaxOutputTokens)
	}

	if cfg.ThinkingConfig == nil || cfg.ThinkingConfig.ThinkingLevel != genai.ThinkingLevelHigh {
		t.Errorf("thinking level = %v, want HIGH", cfg.ThinkingConfig)
	}
}

func TestApplyGenParamsUnsetLeavesConfigUntouched(t *testing.T) {
	t.Parallel()

	cfg := &genai.GenerateContentConfig{}
	agentGenParams{}.applyTo(cfg)

	if cfg.Temperature != nil || cfg.TopP != nil || cfg.MaxOutputTokens != 0 || cfg.ThinkingConfig != nil {
		t.Errorf("expected an untouched config, got %+v", cfg)
	}
}

// floatPtrVal dereferences a *float32, returning a sentinel if nil so callers
// can assert without a nil-guard branch.
func floatPtrVal(p *float32) float32 {
	if p == nil {
		return -1
	}

	return *p
}

// TestRequiredCallSucceeded covers every FunctionResponse shape a
// required:-capable toolImpl can actually produce: shell-backed tools
// (exit_code), MCP tools ({structured_content, content} on success,
// {error} on failure — see mcpToolImpl), and the absent-both-keys case
// (e.g. a builtin's own success shape), which is success by the same
// "no error key present" rule.
func TestRequiredCallSucceeded(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		resp map[string]any
		want bool
	}{
		{"exit_code 0 succeeds", map[string]any{"exit_code": 0, "stdout": "", "stderr": ""}, true},
		{"exit_code nonzero fails", map[string]any{"exit_code": 1, "stdout": "", "stderr": ""}, false},
		{"mcp success shape (no error key) succeeds", map[string]any{"structured_content": nil, "content": "ok"}, true},
		{"mcp error shape fails", map[string]any{"error": "boom"}, false},
		{"neither exit_code nor error present succeeds", map[string]any{"entries": []any{}}, true},
		{"empty map succeeds (no error key)", map[string]any{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := requiredCallSucceeded(tc.resp)
			if got != tc.want {
				t.Errorf("requiredCallSucceeded(%+v) = %v, want %v", tc.resp, got, tc.want)
			}
		})
	}
}

// TestRunAgentConversationRequiredMCPToolCalled proves the regression this
// design exists to prevent: before requiredCallSucceeded was made
// shape-aware, an MCP tool's success shape ({structured_content, content},
// no exit_code) was never recognized as satisfying required: true, so the
// conversation would force-call it every turn and exhaust max_turns even
// though every call actually succeeded. This test registers a hand-built
// toolImpl matching mcpToolImpl's exact success shape (rather than a real
// MCP connection, which this package doesn't have — the fix is entirely in
// requiredCallSucceeded's interpretation of the response map) and confirms
// the conversation completes normally within one call.
func TestRunAgentConversationRequiredMCPToolCalled(t *testing.T) {
	t.Parallel()

	const toolName = "github__search_issues"

	registry := map[string]toolImpl{
		toolName: func(context.Context, map[string]any, toolEnv) map[string]any {
			return map[string]any{"structured_content": map[string]any{"count": 2}, "content": "found 2 issues"}
		},
	}

	conv := agentConversation{
		prompt:   "search for bugs",
		env:      toolEnv{dir: t.TempDir()},
		tools:    agentTools{registry: registry, required: map[string]bool{toolName: true}},
		maxTurns: testMaxTurns,
	}

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: toolName, Args: map[string]any{}}}},
			}},
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("unexpected error (required MCP tool should have been satisfied on its one successful call): %v", err)
	}

	if res.text != "done" {
		t.Errorf("content = %q, want %q", res.text, "done")
	}

	if res.turns != 2 {
		t.Errorf("turns = %d, want 2 (no forced re-call needed once satisfied)", res.turns)
	}
}

// TestRunAgentConversationRequiredMCPToolErrorNotSatisfied confirms the
// other half: an MCP tool's IsError shape ({"error": ...}) must NOT be
// treated as satisfying required:, the same as a nonzero exit_code isn't.
func TestRunAgentConversationRequiredMCPToolErrorNotSatisfied(t *testing.T) {
	t.Parallel()

	const toolName = "github__search_issues"

	registry := map[string]toolImpl{
		toolName: func(context.Context, map[string]any, toolEnv) map[string]any {
			return map[string]any{"error": "upstream rate limited"}
		},
	}

	conv := agentConversation{
		prompt:   "search for bugs",
		env:      toolEnv{dir: t.TempDir()},
		tools:    agentTools{registry: registry, required: map[string]bool{toolName: true}},
		maxTurns: testMaxTurns,
	}

	call := &genai.Part{FunctionCall: &genai.FunctionCall{ID: "call", Name: toolName, Args: map[string]any{}}}

	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{call}}}
	}

	fake := &fakeLLM{responses: responses}

	_, err := runAgentConversation(context.Background(), fake, conv)
	if err == nil {
		t.Fatal("expected an error: the required tool only ever returned an error shape, so it never succeeded")
	}
}
