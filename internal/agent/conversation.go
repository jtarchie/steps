package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

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

// agentConversation is one runnable attempt's inputs.
type agentConversation struct {
	system   string
	prompt   string
	dir      string
	tools    agentTools
	params   agentGenParams
	maxTurns int
	// toolChoiceStringOnly forces a required tool call via the string
	// tool_choice: "required" instead of a named function object — see
	// forceRequiredTool. Set from config.ResolvedInvocation.StringOnlyToolChoice.
	toolChoiceStringOnly bool
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
// terminating when the model responds with no tool calls while every
// required tool has succeeded. There is no turn-level checkpointing — a
// retry (see retry.Do in RunStep/RunFix) restarts the whole conversation from
// scratch, but that only ever happens for a non-tool failure (generateOnce
// erroring, or maxTurns exhausted below): a tool failure, required or not,
// is always handed back into this same conversation as data, never a reason
// to abort it. If a tool call already had a side effect (e.g. posting a PR
// review) before a later turn failed for an unrelated reason, a restart may
// re-invoke it again; pipeline prompts should tell the model to check
// current state before acting, the same caveat Concourse's own
// task.attempts carries for non-idempotent tasks.
//
// If the model tries to stop without every required tool having succeeded
// (exit_code 0 — a failed call doesn't count, and the model already saw why
// it failed via the appended tool response), its next turn is constrained
// via the provider's tool_choice (see forceRequiredTool) to a function call
// for one unsatisfied tool — a hard API-level constraint the model cannot
// decline, not a text reminder it could still ignore. One unsatisfied tool
// is forced per turn (repeated as needed for more than one), still bounded
// by maxTurns: a provider that doesn't honor tool_choice, or a model that
// keeps failing the call anyway, still hits the turn cap rather than
// looping forever.
//
// conv.toolChoiceStringOnly swaps that named-function force for the generic
// tool_choice: "required" string form — some OpenAI-compat local servers
// (e.g. LM Studio) 400 on the named-object form entirely. The generic form
// only guarantees *some* tool call, not the missing one specifically, so a
// model can still stall on the wrong tool until maxTurns; that's the same
// safety bound already documented above.
func runAgentConversation(ctx context.Context, llm model.LLM, conv agentConversation) (string, int, error) {
	req := buildAgentRequest(conv)
	satisfied := make(map[string]bool, len(conv.tools.required))

	for turn := range conv.maxTurns {
		resp, err := generateOnce(ctx, llm, req)
		if err != nil {
			return "", turn, err
		}

		req.Contents = append(req.Contents, resp.Content)

		calls, text := collectParts(resp.Content)
		if len(calls) == 0 {
			missing := unsatisfiedRequiredTools(conv.tools.required, satisfied)
			if len(missing) == 0 {
				return text, turn + 1, nil
			}

			forceRequiredTool(req, missing[0], conv.toolChoiceStringOnly)

			continue
		}

		req.Config.ToolConfig = nil // clear any forcing from the prior turn — the model chooses freely again next time it tries to stop

		parts := toolResponseParts(ctx, calls, conv.dir, conv.tools.registry)

		for _, part := range parts {
			name := part.FunctionResponse.Name
			if conv.tools.required[name] && requiredCallSucceeded(part.FunctionResponse.Response) {
				satisfied[name] = true
			}
		}

		req.Contents = append(req.Contents, &genai.Content{
			Role:  genai.RoleUser,
			Parts: parts,
		})
	}

	missing := unsatisfiedRequiredTools(conv.tools.required, satisfied)
	if len(missing) > 0 {
		return "", conv.maxTurns, fmt.Errorf("agent exceeded %d turns; required tool(s) never succeeded: %s", conv.maxTurns, strings.Join(missing, ", "))
	}

	return "", conv.maxTurns, fmt.Errorf("agent exceeded %d turns without a final response", conv.maxTurns)
}

// forceRequiredTool constrains req's next generateOnce call to a tool call,
// via the provider's tool_choice mechanism (see genaiopenai's ToolConfig →
// tool_choice mapping) — the model cannot decline or reply with plain text
// on that turn. Cleared once any tool call comes back (see
// runAgentConversation) so later turns aren't stuck forced to the same
// name.
//
// stringOnly == false (the default) names name specifically, mapping to
// OpenAI's named tool_choice object — the precise force. stringOnly == true
// leaves AllowedFunctionNames empty, which genaiopenai instead maps to the
// generic string tool_choice: "required" — some OpenAI-compat local servers
// (e.g. LM Studio) reject the named-object form outright.
func forceRequiredTool(req *model.LLMRequest, name string, stringOnly bool) {
	fcc := &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}
	if !stringOnly {
		fcc.AllowedFunctionNames = []string{name}
	}

	req.Config.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: fcc}
}

// requiredCallSucceeded reports whether a tool's FunctionResponse data
// represents success: exit_code 0. Only shell-backed tools (run_shell,
// custom tools) can be marked required:, and shellToolResult always sets
// exit_code on anything that actually ran, so its absence means the command
// never ran at all — also not success.
func requiredCallSucceeded(resp map[string]any) bool {
	code, ok := resp["exit_code"].(int)

	return ok && code == 0
}

// unsatisfiedRequiredTools returns a sorted list of names present in
// required but not yet in satisfied, or nil if none remain. A tool stays
// unsatisfied across as many failed calls as it takes — see
// requiredCallSucceeded — so the model can keep retrying it in-session
// instead of the whole attempt being aborted.
func unsatisfiedRequiredTools(required, satisfied map[string]bool) []string {
	missing := make([]string, 0, len(required))

	for name := range required {
		if !satisfied[name] {
			missing = append(missing, name)
		}
	}

	sort.Strings(missing)

	return missing
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
// FunctionResponse parts to feed back on the next turn. No call's failure
// (a nonzero exit_code, or an {"error": ...} result) stops any other call in
// the turn, or the conversation — every failure is just data the model sees
// on its next turn and can react to.
func toolResponseParts(ctx context.Context, calls []*genai.FunctionCall, dir string, registry map[string]toolImpl) []*genai.Part {
	parts := make([]*genai.Part, 0, len(calls))

	for _, call := range calls {
		response := executeAgentTool(ctx, call, dir, registry)

		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: response},
		})
	}

	return parts
}
