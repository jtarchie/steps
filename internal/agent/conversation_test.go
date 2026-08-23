package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
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

// paddedShellCall returns a model turn requesting run_shell with a harmless
// command whose trailing comment is padded to approximately tokenChars/4
// estimated tokens (via compaction.go's len/4 heuristic) — a deterministic
// way to inflate a turn's estimated size without depending on real command
// output, which would make the exact call count these tests assert on
// fragile (cat's byte count, trailing newlines, etc.).
func paddedShellCall(id string, tokenChars int) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID:   id,
			Name: "run_shell",
			Args: map[string]any{"command": "true # " + strings.Repeat("x", tokenChars)},
		}}},
	}}
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}}
}

// newTestConversation builds an agentConversation with the default grant
// plus run_shell for exercising the loop against a fakeLLM.
func newTestConversation(t *testing.T, prompt, dir string) agentConversation {
	t.Helper()

	specs := append(config.DefaultAgentToolSpecs(), config.ToolSpec{Builtin: "run_shell"})

	built, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		messages: []string{prompt},
		env:      toolEnv{dir: dir, runner: runner},
		tools:    built,
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

	// Each turn's call must differ from the last: an IDENTICAL call with an
	// identical result is what the loop detector (loop.go) watches for, and
	// it would kill the conversation before maxTurns — which is what this
	// test is for. Varying the (harmless) command keeps every interaction's
	// signature distinct.
	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = &model.LLMResponse{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
					ID:   "call1",
					Name: "run_shell",
					Args: map[string]any{"command": fmt.Sprintf("true # turn %d", i)},
				}}},
			},
		}
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

	built, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		messages: []string{"review it"},
		env:      toolEnv{dir: dir, runner: runner},
		tools:    built,
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

	// maxIgnoredForces, not testMaxTurns: a model that has declined an
	// API-level forced tool call five turns running is not going to comply on
	// the sixth, so the attempt ends there instead of buying the same verdict
	// three turns later. The verdict itself is unchanged — that is what makes
	// the earlier stop free.
	//
	// It stopped being optional when max_turns: 0 became expressible: the turn
	// cap used to be the only thing bounding this path, and an uncapped step
	// has none.
	if res.turns != maxIgnoredForces {
		t.Errorf("turns = %d, want %d", res.turns, maxIgnoredForces)
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

	built, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	conv := agentConversation{
		messages: []string{"review it"},
		env:      toolEnv{dir: dir, runner: runner},
		tools:    built,
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
		messages: []string{"search for bugs"},
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
		messages: []string{"search for bugs"},
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

// TestRunAgentConversationCompactsWhenOverBudget confirms compaction is
// actually wired into the live loop, not just correct in isolation
// (compaction_test.go covers the algorithm itself). One padded tool call
// pushes req.Contents comfortably past a small compactAfterTokens budget;
// the next turn's maybeCompact check must fire before that turn's own
// generateOnce call, consuming an extra, interleaved LLM call for the
// summarization itself.
func TestRunAgentConversationCompactsWhenOverBudget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: []*model.LLMResponse{
		paddedShellCall("call1", 4000),                         // turn 0's own generateOnce (index 0)
		textResponse("Summary: ran one shell command so far."), // the interleaved summarization call (index 1)
		textResponse("done"),                                   // turn 1's own generateOnce, now compacted (index 2)
	}}

	conv := newTestConversation(t, "read some things", dir)
	conv.compactAfterTokens = 500 // well under the ~1000-token padded call

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.text != "done" {
		t.Errorf("text = %q, want %q", res.text, "done")
	}

	if len(fake.requests) != 3 {
		t.Fatalf("got %d LLM calls, want 3 (turn 0, the summarization call, turn 1)", len(fake.requests))
	}

	summarizeReq := fake.requests[1]
	if !hasTextContaining(summarizeReq.Contents, "Provide a detailed summary") {
		t.Error("the second LLM call does not look like the summarization request")
	}

	finalReq := fake.requests[2]
	if !hasTextContaining(finalReq.Contents, "[Previous conversation summary]") {
		t.Error("the post-compaction request does not carry the summary")
	}

	if hasFunctionCallWithArgContaining(finalReq.Contents, "command", "xxxx") {
		t.Error("the post-compaction request still contains the original padded tool call -- compaction did not shrink it")
	}
}

// TestRunAgentConversationCompactionPreservesToolPairBoundary drives three
// padded tool-call turns whose natural (pre-pair-adjustment) split point
// lands exactly on a FunctionCall entry -- the case walkBackToPairBoundary
// treats conservatively: since call+response always occupy adjacent Content
// entries in this codebase (see toolResponseParts), a call at the split
// point already has its response safely on the recent side, but the walk
// doesn't know that locally and retreats past every earlier pair anyway,
// landing (via the forward-walk fallback) at len(contents) -- everything
// summarized, nothing left "recent." That's conservative rather than
// precise, and this test's real point: confirm it's conservative-SAFE, not
// conservative-buggy -- every request captured across the run, pre- and
// post-compaction, keeps every FunctionCall paired with its
// FunctionResponse.
func TestRunAgentConversationCompactionPreservesToolPairBoundary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: []*model.LLMResponse{
		paddedShellCall("call1", 400),                      // turn 0 (index 0)
		paddedShellCall("call2", 400),                      // turn 1 (index 1)
		paddedShellCall("call3", 400),                      // turn 2 (index 2) -- pushes over budget
		textResponse("Summary: ran three shell commands."), // interleaved summarization (index 3)
		textResponse("done"),                               // turn 3, now compacted (index 4)
	}}

	conv := newTestConversation(t, "read three things", dir)
	conv.compactAfterTokens = 250 // triggers partway through the third call/response pair

	_, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	for i, req := range fake.requests {
		if hasDanglingFunctionCall(req.Contents) {
			t.Errorf("request %d has a FunctionCall with no matching FunctionResponse", i)
		}
	}
}

// TestRunAgentConversationCompactionFailurePassesThrough confirms a failed
// summarization call is logged and passed through -- the conversation keeps
// going to a normal completion, exactly like any other tool/data failure in
// this loop, never an aborted attempt.
func TestRunAgentConversationCompactionFailurePassesThrough(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			paddedShellCall("call1", 4000), // turn 0 (index 0)
			nil,                            // the summarization call (index 1) -- errs[1] fires instead
			textResponse("done"),           // turn 1, uncompacted (index 2)
		},
		errs: []error{nil, errors.New("summarizer unavailable")},
	}

	conv := newTestConversation(t, "read some things", dir)
	conv.compactAfterTokens = 500

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.text != "done" {
		t.Errorf("text = %q, want %q -- a failed compaction must not abort the attempt", res.text, "done")
	}

	if len(fake.requests) != 3 {
		t.Fatalf("got %d LLM calls, want 3 (turn 0, the failed summarization attempt, turn 1)", len(fake.requests))
	}

	if hasTextContaining(fake.requests[2].Contents, "[Previous conversation summary]") {
		t.Error("req.Contents was rewritten despite the summarization call failing")
	}
}

// TestRunAgentConversationCompactionDisabledWhenZero is the value-gating
// regression test: compactAfterTokens at its zero value (an agent that set
// compact_after_tokens: 0) must behave exactly as the loop did before
// compaction existed -- req.Contents grows monotonically, no summary marker
// ever appears, and the LLM is called exactly once per turn.
func TestRunAgentConversationCompactionDisabledWhenZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: []*model.LLMResponse{
		paddedShellCall("call1", 4000),
		paddedShellCall("call2", 4000),
		textResponse("done"),
	}}

	conv := newTestConversation(t, "read some things", dir)
	conv.compactAfterTokens = 0 // explicit disable, not just "small"

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.text != "done" {
		t.Errorf("text = %q, want %q", res.text, "done")
	}

	if len(fake.requests) != 3 {
		t.Fatalf("got %d LLM calls, want exactly 3 (one per turn, no interleaved summarization)", len(fake.requests))
	}

	prevLen := 0

	for i, req := range fake.requests {
		if hasTextContaining(req.Contents, "[Previous conversation summary]") {
			t.Errorf("request %d carries a summary marker despite compaction being disabled", i)
		}

		if len(req.Contents) < prevLen {
			t.Errorf("request %d has fewer Contents (%d) than the previous request (%d) -- history was not supposed to shrink", i, len(req.Contents), prevLen)
		}

		prevLen = len(req.Contents)
	}
}

func hasTextContaining(contents []*genai.Content, substr string) bool {
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.Text != "" && strings.Contains(p.Text, substr) {
				return true
			}
		}
	}

	return false
}

func hasFunctionCallWithArgContaining(contents []*genai.Content, argKey, substr string) bool {
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionCall == nil {
				continue
			}

			v, ok := p.FunctionCall.Args[argKey].(string)
			if ok && strings.Contains(v, substr) {
				return true
			}
		}
	}

	return false
}

// hasDanglingFunctionCall reports whether contents contains a FunctionCall
// with no matching FunctionResponse (by call ID) anywhere in the same slice
// -- the malformed-request shape a strict OpenAI-compatible API rejects.
func hasDanglingFunctionCall(contents []*genai.Content) bool {
	responseIDs := make(map[string]bool)

	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				responseIDs[p.FunctionResponse.ID] = true
			}
		}
	}

	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil && !responseIDs[p.FunctionCall.ID] {
				return true
			}
		}
	}

	return false
}

// TestOutOfTurnsSurfacesAWrapUpFailure pins the classification of a wrap-up
// request that itself fails.
//
// When the turn budget runs out, the runner takes the tools away and asks the
// model to answer from what it gathered. If THAT request fails — a 503, or a
// token ceiling breached by it — the cause has to survive: the error is
// infrastructure, so it must stay unmarked (→ errored, firing on_error), not
// be replaced by an outcome.Fail claiming the model "produced none". It used
// to be swallowed entirely, reporting a provider outage as a task failure.
func TestOutOfTurnsSurfacesAWrapUpFailure(t *testing.T) {
	t.Parallel()

	// Every turn calls a tool, so the loop runs the budget out; the wrap-up
	// request that follows is the one that errors. Each command differs, or
	// the loop detector would end the conversation before the turns do.
	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = distinctShellCall(i)
	}

	errs := make([]error, testMaxTurns+1)
	errs[testMaxTurns] = errors.New("provider is down")

	fake := &fakeLLM{responses: responses, errs: errs}

	_, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "investigate", t.TempDir()))
	if err == nil {
		t.Fatal("expected an error when the wrap-up request fails")
	}

	if !strings.Contains(err.Error(), "provider is down") {
		t.Errorf("the wrap-up failure's cause was swallowed; got %v", err.Error())
	}

	var failure *outcome.Failure
	if errors.As(err, &failure) {
		t.Error("a failed wrap-up request classified as a task failure; it is infrastructure and must classify as errored")
	}
}

// TestOutOfTurnsAnswersWithoutTools is the success half: the wrap-up answer
// becomes the result, and is marked so a degraded answer is tellable from a
// confident one.
func TestOutOfTurnsAnswersWithoutTools(t *testing.T) {
	t.Parallel()

	responses := make([]*model.LLMResponse, testMaxTurns+1)
	for i := range responses[:testMaxTurns] {
		responses[i] = distinctShellCall(i)
	}

	responses[testMaxTurns] = &model.LLMResponse{Content: &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{Text: "what I found before running out"}},
	}}

	fake := &fakeLLM{responses: responses}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "investigate", t.TempDir()))
	if err != nil {
		t.Fatalf("a spent turn budget destroyed the work instead of ending it: %v", err)
	}

	if res.text != "what I found before running out" {
		t.Errorf("text = %q, want the wrap-up answer", res.text)
	}

	if !res.wrappedUp {
		t.Error("the answer is not marked as produced against a spent budget")
	}

	// The last request must offer no tools — a model that spent every turn
	// calling them has already shown it will not stop when merely asked.
	last := fake.requests[len(fake.requests)-1]
	if last.Config != nil && len(last.Config.Tools) > 0 {
		t.Error("the wrap-up request still offered tools")
	}
}

// TestMarkTrajectoryResults checks the success backfill, including the
// length-mismatch degrade: pairing a call with the wrong result would
// misattribute what actually happened.
func TestMarkTrajectoryResults(t *testing.T) {
	t.Parallel()

	turn := []recordedToolCall{{name: "a"}, {name: "b"}}
	parts := []*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{Name: "a", Response: map[string]any{"exit_code": 0}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "b", Response: map[string]any{"error": "nope"}}},
	}

	markTrajectoryResults(turn, parts)

	if !turn[0].ok || turn[1].ok {
		t.Errorf("ok flags = [%v %v], want [true false]", turn[0].ok, turn[1].ok)
	}

	mismatched := []recordedToolCall{{name: "a"}, {name: "b"}}
	markTrajectoryResults(mismatched, parts[:1])

	if mismatched[0].ok || mismatched[1].ok {
		t.Error("a length mismatch must leave every call unmarked rather than mispairing")
	}
}

// distinctShellCall is a run_shell turn whose command is unique to i, so a
// test that needs the TURN budget to run out is not ended early by the loop
// detector (which fails a conversation repeating one identical call).
func distinctShellCall(i int) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{
				ID:   fmt.Sprintf("call%d", i),
				Name: "run_shell",
				Args: map[string]any{"command": fmt.Sprintf("echo step-%d", i)},
			}},
		},
	}}
}
