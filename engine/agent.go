package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
)

const (
	appName = "steps"
	userID  = "steps"
)

// runAgent executes an agent state: fresh (or adopted) conversation, ADK
// agent loop, engine-owned output contract, semantic retries with feedback.
// Transient model errors bubble up to the engine's retry driver. extraData
// carries foreach item data ({as}: item, index, total).
func (e *Engine) runAgent(ctx context.Context, m *machine.Machine, st *machine.State, runID string, rs *journal.RunState, extraData map[string]any) (*HandlerResult, error) {
	spec := st.Agent

	// The model ref may be routed at runtime (model: {expr: ...}).
	modelRef, err := e.resolveModelRef(m, st, rs, extraData)
	if err != nil {
		return nil, err
	}

	// Template data: ctx, foreach item data, optional history projection.
	extra := map[string]any{}
	for k, v := range extraData {
		extra[k] = v
	}
	if h := spec.History; h != nil {
		msgs := rs.Convos[h.From]
		if len(msgs) == 0 {
			e.Listener.Warn("history source has no recorded execution", "state", st.Name, "from", h.From)
		}
		extra[h.As] = renderHistory(msgs, h)
	}
	data := templateData(rs, extra)

	// The user message: the rendered prompt, or the rendered input block.
	userMsg, err := e.buildUserMessage(st, data)
	if err != nil {
		return nil, err
	}

	system, err := e.buildSystemInstruction(st, data)
	if err != nil {
		return nil, err
	}

	// Memoization: byte-identical rendered input (model + system + prompt)
	// replays the cached output — re-runs only re-pay for what changed.
	memoKey := ""
	if st.Memo {
		memoKey = memoHash(modelRef, system, userMsg)
		if cached, ok, memoErr := e.Store.MemoGet(ctx, memoKey); memoErr == nil && ok {
			e.Listener.MemoHit(st.Name)
			return &HandlerResult{Output: cached, Memo: true}, nil
		}
	}

	// Model client: the mock script (when set) replaces every provider.
	var llm adkmodel.LLM
	if e.Mock != nil {
		llm = e.mockForRun(runID).ForState(st.Name)
	} else {
		llm, err = e.resolveLLM(modelRef)
		if err != nil {
			return nil, &provider.ClassifiedError{Class: machine.ClassProviderError, Msg: err.Error()}
		}
	}

	// Fresh conversation per state — hermetic by default. adopt (rung 3)
	// replays a prior execution's normalized messages into the session.
	svc := session.InMemoryService()
	created, err := svc.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}
	sess := created.Session
	agentName := sanitizeName(st.Name)

	if spec.Adopt != "" {
		target := spec.Adopt
		if target == "self" {
			target = st.Name
		}
		prior := rs.Convos[target]
		if len(prior) == 0 && spec.Adopt != "self" {
			// Adopting a state that never executed is a semantic failure —
			// never a silent fresh start. adopt: self on first visit starts
			// fresh by definition.
			return nil, &provider.ClassifiedError{
				Class: machine.ClassAdoptMissing,
				Msg:   fmt.Sprintf("state %q adopts %q, which has not executed this run", st.Name, spec.Adopt),
			}
		}
		// Scratch reasoning is not context: replaying it re-bills it and
		// anchors the model to stale thinking. Only real exchanges replay.
		trimmed := make([]journal.Message, 0, len(prior))
		for _, msg := range prior {
			if !msg.Thought {
				trimmed = append(trimmed, msg)
			}
		}
		if n := spec.AdoptLastTurns; n > 0 && len(trimmed) > n {
			trimmed = trimmed[len(trimmed)-n:]
		}
		for _, msg := range trimmed {
			if err := svc.AppendEvent(ctx, sess, adoptEvent(agentName, msg)); err != nil {
				return nil, fmt.Errorf("seeding adopted conversation: %w", err)
			}
		}
	}

	// Callbacks: usage accounting, turn budget, chat narration. The turn
	// budget bounds model calls within ONE conversation turn (the tool loop);
	// it resets before each driveTurn so semantic retries get a fresh budget.
	usage := &journal.Usage{}
	turns := 0
	maxTurns := spec.MaxTurns
	before := func(cctx adkagent.CallbackContext, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		turns++
		if turns > maxTurns {
			return nil, &provider.ClassifiedError{
				Class: machine.ClassBudgetExceeded,
				Msg:   fmt.Sprintf("agent exceeded max_turns %d", maxTurns),
			}
		}
		return nil, nil
	}
	after := func(cctx adkagent.CallbackContext, resp *adkmodel.LLMResponse, respErr error) (*adkmodel.LLMResponse, error) {
		if resp != nil && resp.UsageMetadata != nil {
			usage.InputTokens += int(resp.UsageMetadata.PromptTokenCount)
			usage.OutputTokens += int(resp.UsageMetadata.CandidatesTokenCount)
		}
		return nil, nil
	}

	tools, beforeTool, afterTool, err := e.buildAgentTools(st, rs, &turns)
	if err != nil {
		return nil, err
	}

	genCfg := &genai.GenerateContentConfig{}
	if spec.Temperature != nil {
		genCfg.Temperature = genai.Ptr(float32(*spec.Temperature))
	}
	// No state may generate unboundedly: a runaway or grammar-degenerate
	// completion becomes a bounded failure, never a hang.
	genCfg.MaxOutputTokens = int32(spec.MaxOutputTokens)
	// Reasoning tokens are billed output; each micro-agent declares how much
	// thinking its one job deserves (OpenAI-compatible: reasoning_effort).
	if spec.Reasoning != "" {
		genCfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: thinkingLevel(spec.Reasoning)}
	}
	// structured_output: native constrains the decoder itself on providers
	// that support it (OpenAI-compatible maps this to response_format
	// json_schema). Opt-in: a token win on well-supported backends, but
	// grammar-constrained sampling degenerates on some local model/backend
	// combos. Gated to tool-less states — constrained decoding and tool
	// calls conflict on several backends. The prompt contract always applies.
	if spec.StructuredOutput == "native" && len(spec.Tools) == 0 && !plainTextContract(st.Output) {
		genCfg.ResponseMIMEType = "application/json"
		genCfg.ResponseSchema = machine.GenaiSchema(st.Output.Schema, st.Output.Events)
	}

	ag, err := llmagent.New(llmagent.Config{
		Name:        agentName,
		Description: "steps state " + st.Name,
		Model:       llm,
		InstructionProvider: func(adkagent.ReadonlyContext) (string, error) {
			return system, nil
		},
		GenerateContentConfig: genCfg,
		Tools:                 tools,
		BeforeModelCallbacks:  []llmagent.BeforeModelCallback{before},
		AfterModelCallbacks:   []llmagent.AfterModelCallback{after},
		BeforeToolCallbacks:   []llmagent.BeforeToolCallback{beforeTool},
		AfterToolCallbacks:    []llmagent.AfterToolCallback{afterTool},

		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("building agent: %w", err)
	}

	r, err := runner.New(runner.Config{AppName: appName, Agent: ag, SessionService: svc})
	if err != nil {
		return nil, fmt.Errorf("building runner: %w", err)
	}

	// The conversation driver. Semantic (schema) violations retry with the
	// validation error appended to the SAME conversation, so the model can
	// correct itself. Bounded by the state's semantic retry policy.
	semanticAttempts := 0
	msg := userMsg
	for {
		turns = 0 // the budget is per conversation turn, not per handler
		e.Listener.AgentMessage(st.Name, "user", msg)
		texts, runErr := e.driveTurn(ctx, r, st.Name, sess.ID(), msg)
		if runErr != nil {
			return nil, runErr // transient: engine retry driver replays the handler
		}
		finalText := chooseFinalText(texts, st.Output)

		output, event, parseErr := parseOutput(finalText, st.Output)
		if parseErr == nil {
			messages, err := e.collectMessages(ctx, svc, sess.ID())
			if err != nil {
				e.Listener.Warn("could not collect conversation for journal", "error", err.Error())
			}
			if memoKey != "" {
				if err := e.Store.MemoPut(ctx, memoKey, output); err != nil {
					e.Listener.Warn("memo store failed", "state", st.Name, "error", err.Error())
				}
			}
			return &HandlerResult{
				Output:   output,
				Event:    event,
				Usage:    *usage,
				Messages: messages,
			}, nil
		}

		semanticAttempts++
		e.Listener.HandlerFailed(st.Name, machine.ClassSchemaViolation, parseErr, semanticAttempts)
		_ = e.append(ctx, runID, journal.HandlerFailed, map[string]any{
			"state": st.Name, "class": machine.ClassSchemaViolation,
			"error": parseErr.Error(), "attempt": semanticAttempts,
		})

		var policy *machine.RetryPolicy
		for i := range st.Retry {
			if st.Retry[i].Matches(machine.ClassSchemaViolation) {
				policy = &st.Retry[i]
				break
			}
		}
		if policy == nil || semanticAttempts >= policy.MaxAttempts {
			return nil, &provider.ClassifiedError{Class: machine.ClassSchemaViolation, Msg: parseErr.Error()}
		}
		_ = e.append(ctx, runID, journal.RetryScheduled, map[string]any{
			"state": st.Name, "class": machine.ClassSchemaViolation, "attempt": semanticAttempts + 1,
		})
		e.Listener.RetryScheduled(st.Name, machine.ClassSchemaViolation, semanticAttempts+1, 0)

		msg = fmt.Sprintf(
			"Your response did not satisfy the output contract: %s\n\nReply again with ONLY a corrected JSON object. No prose, no markdown fences.",
			parseErr,
		)
	}
}

// driveTurn runs one runner invocation and returns every model text part, in
// order. The caller picks the authoritative one against the output contract.
func (e *Engine) driveTurn(ctx context.Context, r *runner.Runner, state, sessionID, msg string) ([]string, error) {
	var texts []string
	var lastFinish genai.FinishReason
	for ev, err := range r.Run(ctx, userID, sessionID, genai.NewContentFromText(msg, genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			return nil, err
		}
		if ev == nil {
			continue
		}
		if ev.FinishReason != "" {
			lastFinish = ev.FinishReason
		}
		if ev.Content == nil || ev.Partial {
			continue
		}
		for _, part := range ev.Content.Parts {
			switch {
			case part.Text != "" && part.Thought:
				// Reasoning channel: narrated, never a contract candidate.
				e.Listener.AgentMessage(state, "thought", part.Text)
			case part.Text != "" && ev.Author != "user":
				e.Listener.AgentMessage(state, "model", part.Text)
				texts = append(texts, part.Text)
			case part.FunctionCall != nil:
				e.Listener.ToolCalled(state, part.FunctionCall.Name, part.FunctionCall.Args)
			case part.FunctionResponse != nil:
				e.Listener.ToolResult(state, part.FunctionResponse.Name, part.FunctionResponse.Response)
			}
		}
	}
	if len(texts) == 0 {
		if lastFinish == genai.FinishReasonMaxTokens {
			// Deterministic, not transient: the output cap was exhausted
			// (typically inside the reasoning channel) before any content.
			return nil, &provider.ClassifiedError{
				Class: machine.ClassBudgetExceeded,
				Msg:   "max_output_tokens exhausted before any content — raise it or lower reasoning",
			}
		}
		return nil, &provider.ClassifiedError{Class: machine.ClassProviderError, Msg: "model produced no text"}
	}
	return texts, nil
}

// chooseFinalText picks the authoritative reply. For JSON contracts, prefer
// the LAST part that actually parses — models sometimes emit reasoning before
// (or remarks after) the JSON, and position is not a reliable signal.
func chooseFinalText(texts []string, spec machine.OutputSpec) string {
	if len(texts) == 0 {
		return ""
	}
	if !plainTextContract(spec) {
		for i := len(texts) - 1; i >= 0; i-- {
			if _, err := extractJSON(texts[i]); err == nil {
				return texts[i]
			}
		}
	}
	return texts[len(texts)-1]
}

// buildUserMessage renders the prompt template, or the input block as a
// labeled message when no prompt is declared.
func (e *Engine) buildUserMessage(st *machine.State, data map[string]any) (string, error) {
	if st.Agent.Prompt != "" {
		return machine.RenderTemplate(st.Name+".prompt", st.Agent.Prompt, data)
	}
	var b strings.Builder
	for _, k := range sortedKeys(st.Input) {
		rendered, err := machine.RenderTemplate(st.Name+".input."+k, st.Input[k], data)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s:\n%s\n\n", k, rendered)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// buildSystemInstruction renders the system template and appends the output
// contract. The contract is engine-owned: it works identically across
// providers, including small local models with no JSON mode.
func (e *Engine) buildSystemInstruction(st *machine.State, data map[string]any) (string, error) {
	var parts []string
	if st.Agent.System != "" {
		system, err := machine.RenderTemplate(st.Name+".system", st.Agent.System, data)
		if err != nil {
			return "", err
		}
		parts = append(parts, system)
	}

	if !plainTextContract(st.Output) {
		schemaJSON, err := machine.SchemaJSON(st.Output.Schema, st.Output.Events)
		if err != nil {
			return "", err
		}
		contract := "Reply with a single JSON object matching this schema: " + schemaJSON +
			"\nBegin your reply with { and end with }. No analysis, no preamble, no prose, no markdown fences."
		if len(st.Output.Events) > 0 {
			contract += fmt.Sprintf("\nSet \"event\" to exactly one of %v — it declares your conclusion and routes the workflow.", st.Output.Events)
		}
		parts = append(parts, contract)
	}
	if len(parts) == 0 {
		return "You are a precise assistant completing one specific task.", nil
	}
	return strings.Join(parts, "\n\n"), nil
}

// plainTextContract: the implicit {text: string} output with no events needs
// no JSON — the raw reply is the output.
func plainTextContract(o machine.OutputSpec) bool {
	return o.DefaultOutput() && len(o.Events) == 0
}

// parseOutput validates the model's reply against the state contract.
func parseOutput(text string, spec machine.OutputSpec) (map[string]any, string, error) {
	if plainTextContract(spec) {
		return map[string]any{"text": strings.TrimSpace(text)}, "", nil
	}
	raw, err := extractJSON(text)
	if err != nil {
		return nil, "", err
	}
	if spec.Compiled != nil {
		if err := spec.Compiled.Validate(raw); err != nil {
			return nil, "", fmt.Errorf("schema validation: %w", err)
		}
	}
	event, _ := raw["event"].(string)
	return raw, event, nil
}

// extractJSON pulls a JSON object out of model text: reasoning blocks are
// stripped, fenced blocks preferred, then the widest brace span.
func extractJSON(text string) (map[string]any, error) {
	text = stripThinking(text)

	candidates := []string{strings.TrimSpace(text)}
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		if j := strings.Index(rest, "```"); j >= 0 {
			candidates = append([]string{strings.TrimSpace(rest[:j])}, candidates...)
		}
	}
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		candidates = append(candidates, text[i:j+1])
	}

	var lastErr error
	for _, c := range candidates {
		var out map[string]any
		if err := json.Unmarshal([]byte(c), &out); err == nil {
			return out, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no JSON object found")
	}
	return nil, fmt.Errorf("expected a JSON object: %w", lastErr)
}

func stripThinking(text string) string {
	for {
		start := strings.Index(text, "<think>")
		if start < 0 {
			return text
		}
		end := strings.Index(text[start:], "</think>")
		if end < 0 {
			return text[:start]
		}
		text = text[:start] + text[start+end+len("</think>"):]
	}
}

// collectMessages normalizes the session conversation for the journal —
// history renders it, adopt replays it.
func (e *Engine) collectMessages(ctx context.Context, svc session.Service, sessionID string) ([]journal.Message, error) {
	resp, err := svc.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	var out []journal.Message
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		msg := journal.Message{Role: "model"}
		if ev.Author == "user" {
			msg.Role = "user"
		}
		var thought journal.Message
		for _, part := range ev.Content.Parts {
			switch {
			case part.Text != "" && part.Thought:
				// Audit-only: kept as its own flagged message so adopt
				// and history can skip it cheaply.
				if thought.Text != "" {
					thought.Text += "\n"
				}
				thought.Role, thought.Thought = msg.Role, true
				thought.Text += part.Text
			case part.Text != "":
				if msg.Text != "" {
					msg.Text += "\n"
				}
				msg.Text += part.Text
			case part.FunctionCall != nil:
				msg.ToolCalls = append(msg.ToolCalls, journal.ToolCall{
					Name: part.FunctionCall.Name, Args: part.FunctionCall.Args,
				})
			case part.FunctionResponse != nil:
				msg.ToolResults = append(msg.ToolResults, journal.ToolResult{
					Name: part.FunctionResponse.Name, Result: part.FunctionResponse.Response,
				})
			}
		}
		if thought.Text != "" {
			out = append(out, thought)
		}
		if msg.Text != "" || len(msg.ToolCalls) > 0 || len(msg.ToolResults) > 0 {
			out = append(out, msg)
		}
	}
	return out, nil
}

// adoptEvent converts a normalized journal message back into a session event.
func adoptEvent(agentName string, msg journal.Message) *session.Event {
	ev := session.NewEvent("adopt")
	role := genai.RoleModel
	ev.Author = agentName
	if msg.Role == "user" {
		role = genai.RoleUser
		ev.Author = "user"
	}
	var parts []*genai.Part
	if msg.Text != "" {
		parts = append(parts, genai.NewPartFromText(msg.Text))
	}
	for _, tc := range msg.ToolCalls {
		parts = append(parts, genai.NewPartFromFunctionCall(tc.Name, tc.Args))
	}
	for _, tr := range msg.ToolResults {
		parts = append(parts, genai.NewPartFromFunctionResponse(tr.Name, tr.Result))
	}
	ev.Content = &genai.Content{Role: string(role), Parts: parts}
	return ev
}

// buildAgentTools wraps registered tools for the agent loop, with tool
// guards enforced in BeforeToolCallbacks: agent proposes, guards dispose,
// recursively.
func (e *Engine) buildAgentTools(st *machine.State, rs *journal.RunState, turns *int) ([]adktool.Tool, llmagent.BeforeToolCallback, llmagent.AfterToolCallback, error) {
	refs := st.Agent.Tools
	calls := map[string]int{}
	byADKName := map[string]machine.ToolRef{}

	var tools []adktool.Tool
	for _, ref := range refs {
		reg, ok := e.Tools.Get(ref.Name)
		if !ok {
			return nil, nil, nil, fmt.Errorf("state %q: tool %q is not registered", st.Name, ref.Name)
		}
		adkName := sanitizeName(ref.Name)
		byADKName[adkName] = ref
		fn := reg.Fn
		t, err := functiontool.New(functiontool.Config{
			Name:         adkName,
			Description:  reg.Description,
			InputSchema:  &jsonschema.Schema{Type: "object"},
			OutputSchema: &jsonschema.Schema{Type: "object"},
		}, func(cctx adktool.Context, args map[string]any) (map[string]any, error) {
			return fn(cctx, args)
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("wrapping tool %q: %w", ref.Name, err)
		}
		tools = append(tools, t)
	}

	beforeTool := func(cctx adktool.Context, t adktool.Tool, args map[string]any) (map[string]any, error) {
		ref, ok := byADKName[t.Name()]
		if !ok {
			return nil, nil
		}
		e.Listener.ToolCalled(st.Name, ref.Name, args)

		reject := func(reason string) (map[string]any, error) {
			e.Listener.ToolRejected(st.Name, ref.Name, reason, ref.OnReject)
			if ref.OnReject == "fail" {
				return nil, &provider.ClassifiedError{Class: machine.ClassGuardRejected, Msg: reason}
			}
			// feedback: the guard verdict IS the tool result; the model
			// adapts within the loop.
			return map[string]any{"rejected": true, "reason": reason}, nil
		}

		if ref.MaxCalls > 0 && calls[ref.Name] >= ref.MaxCalls {
			return reject(fmt.Sprintf("tool %q exceeded max_calls %d for this state", ref.Name, ref.MaxCalls))
		}
		if ref.Require != "" && calls[ref.Require] == 0 {
			return reject(fmt.Sprintf("tool %q requires %q to have been called first", ref.Name, ref.Require))
		}
		if ref.Guard != nil {
			env := machine.GuardEnv()
			env["ctx"] = rs.Ctx
			env["args"] = args
			env["calls"] = calls
			env["turn"] = *turns
			env["visits"] = rs.Visits
			env["run"] = map[string]any{
				"transitions": rs.Transitions,
				"tokens":      rs.Usage.Total(),
				"cost":        rs.Usage.Cost,
			}
			ok, err := machine.EvalGuard(ref.Guard, env)
			if err != nil {
				e.Listener.Warn("tool guard evaluation failed; rejecting call",
					"state", st.Name, "tool", ref.Name, "error", err.Error())
				return reject(fmt.Sprintf("guard error: %v", err))
			}
			if !ok {
				return reject(fmt.Sprintf("guard %q rejected the call", ref.When))
			}
		}
		calls[ref.Name]++
		return nil, nil
	}

	afterTool := func(cctx adktool.Context, t adktool.Tool, args, result map[string]any, err error) (map[string]any, error) {
		if ref, ok := byADKName[t.Name()]; ok && err == nil {
			e.Listener.ToolResult(st.Name, ref.Name, result)
		}
		return nil, nil
	}

	return tools, beforeTool, afterTool, nil
}

// resolveModelRef returns the state's model ref, evaluating model: {expr}
// routing and resolving aliases.
func (e *Engine) resolveModelRef(m *machine.Machine, st *machine.State, rs *journal.RunState, extraData map[string]any) (string, error) {
	spec := st.Agent
	if spec.ModelExprProgram == nil {
		return spec.Model, nil
	}
	env := machine.GuardEnv()
	env["ctx"] = rs.Ctx
	env["visits"] = rs.Visits
	env["run"] = map[string]any{
		"transitions": rs.Transitions,
		"tokens":      rs.Usage.Total(),
		"cost":        rs.Usage.Cost,
	}
	for k, v := range extraData {
		env[k] = v
	}
	out, err := machine.EvalExpr(spec.ModelExprProgram, env)
	if err != nil {
		return "", &provider.ClassifiedError{Class: machine.ClassProviderError,
			Msg: fmt.Sprintf("model.expr %q: %v", spec.ModelExpr, err)}
	}
	ref, ok := out.(string)
	if !ok {
		return "", &provider.ClassifiedError{Class: machine.ClassProviderError,
			Msg: fmt.Sprintf("model.expr returned %T, want a model alias or ref", out)}
	}
	if resolved, ok := m.Models[ref]; ok {
		ref = resolved
	}
	if !strings.Contains(ref, "/") && ref != "mock" {
		return "", &provider.ClassifiedError{Class: machine.ClassProviderError,
			Msg: fmt.Sprintf("model.expr returned %q — not a models: alias or provider-namespaced ref", ref)}
	}
	return ref, nil
}

// memoHash keys the memo cache on everything that shapes the reply.
func memoHash(modelRef, system, userMsg string) string {
	h := sha256.New()
	for _, part := range []string{modelRef, system, userMsg} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// thinkingLevel maps the YAML reasoning knob to genai's enum.
func thinkingLevel(r string) genai.ThinkingLevel {
	switch r {
	case "low":
		return genai.ThinkingLevelLow
	case "high":
		return genai.ThinkingLevelHigh
	}
	return genai.ThinkingLevelMedium
}

// sanitizeName maps state/tool names to identifiers ADK and providers accept
// (dots are invalid in function names for most providers).
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" || out == "user" {
		out = "state_" + out
	}
	return out
}

// mockForRun returns a per-run mock provider, so queues survive across
// attempts and visits within one run.
func (e *Engine) mockForRun(runID string) *provider.Mock {
	if e.mocks == nil {
		e.mocks = map[string]*provider.Mock{}
	}
	if m, ok := e.mocks[runID]; ok {
		return m
	}
	m := provider.NewMock(e.Mock)
	e.mocks[runID] = m
	return m
}
