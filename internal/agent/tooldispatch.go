package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/agent/llmclient"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"
)

// toolResponseParts executes each requested tool and packages the results as
// FunctionResponse parts to feed back on the next turn. No call's failure
// (a nonzero exit_code, or an {"error": ...} result) stops any other call in
// the turn, or the conversation — every failure is just data the model sees
// on its next turn and can react to. maxCalls/callCounts enforce each
// budgeted tool's max_calls: — see executeBudgetedTool.
func toolResponseParts(ctx context.Context, calls []*genai.FunctionCall, env toolEnv, registry map[string]toolImpl, maxCalls, callCounts map[string]int) []*genai.Part {
	parts := make([]*genai.Part, 0, len(calls))

	for _, call := range calls {
		response := executeBudgetedTool(ctx, call, env, registry, maxCalls, callCounts)

		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: response},
		})
	}

	return parts
}

// executeBudgetedTool enforces call.Name's max_calls: budget (if any) before
// dispatching to executeAgentTool: once callCounts[call.Name] has reached its
// budget, the call is rejected as ordinary tool-result data — never executed,
// since the budget's whole point is bounding side effects — so the model sees
// the rejection and can react on its next turn instead of the attempt
// aborting. Every dispatch increments the counter, success or not; a rejected one
// does not (it's already at the ceiling). Tools absent from maxCalls are
// unlimited.
//
// Every call (budget-rejected or dispatched) is bracketed by a debug-level
// "agent.tool_call"/"agent.tool_result" log pair — the args a call was made
// with, and how it finished (duration, error/exit_code, plus a size-capped
// preview of the rest of the result via debugToolResultPreview — the gap
// that once made a model silently abandoning a granted tool for a worse
// fallback undiagnosable without an out-of-band probe). Debug logging is
// opt-in, so this is silent
// unless an operator has already asked to see command/output content at
// that level; call.Args/the result can hold a model-authored value (e.g. a
// custom tool's args, or write_file's content), no different from the full
// command/output internal/shell already logs at this level.
func executeBudgetedTool(ctx context.Context, call *genai.FunctionCall, env toolEnv, registry map[string]toolImpl, maxCalls, callCounts map[string]int) map[string]any {
	// Which run/job/step/depth this call belongs to — the identity every
	// nested call (sub-agent, MCP, custom tool) shares with its enclosing
	// conversation, read from the recorder rather than threaded as
	// parameters. Zero-valued outside a conversation (tests, direct calls).
	live := env.transcript.liveIdentity()

	slog.DebugContext(ctx, "agent.tool_call", "tool", call.Name, "id", call.ID,
		"run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex, "depth", live.depth,
		"args", call.Args)

	start := time.Now()

	ctx, span := startToolSpan(ctx, call)
	defer span.End()

	var response map[string]any

	if budget, ok := maxCalls[call.Name]; ok && callCounts[call.Name] >= budget {
		response = map[string]any{"error": fmt.Sprintf("%s: call budget (%d) exhausted for this attempt", call.Name, budget)}
	} else {
		response = executeAgentTool(ctx, call, env, registry)
		callCounts[call.Name]++
	}

	slog.DebugContext(ctx, "agent.tool_result", "tool", call.Name, "id", call.ID,
		"run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex, "depth", live.depth,
		"duration", time.Since(start), "error", response["error"], "exit_code", response["exit_code"],
		"result", lazyToolResultPreview{response: response})

	if failure, ok := response["error"]; ok && failure != nil {
		span.SetStatus(codes.Error, fmt.Sprint(failure))
	}

	return response
}

func startToolSpan(ctx context.Context, call *genai.FunctionCall) (context.Context, trace.Span) {
	return otel.Tracer(llmclient.TelemetryScope).Start(ctx, "execute_tool "+call.Name, trace.WithAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", call.Name),
		attribute.String("gen_ai.tool.call.id", call.ID),
	))
}

// lazyToolResultPreview defers rendering a tool result until a handler
// actually formats the record.
//
// Debug logging is off by default, and this line runs on every tool call of
// every turn — building a filtered copy of the map and marshaling it (which
// for a read_file result means touching up to maxReadFileBytes) purely to
// produce a string nothing will print is work proportional to the result, on
// the hot path, in the common case. slog calls LogValue only when the record
// survives level filtering.
type lazyToolResultPreview struct {
	response map[string]any
}

func (p lazyToolResultPreview) LogValue() slog.Value {
	return slog.StringValue(debugToolResultPreview(p.response))
}

// debugToolResultPreviewBytes caps how much of a tool's result content
// appears in the agent.tool_result debug log line: large enough to see
// what a tool actually returned, small enough that a big read_file/
// go_search result doesn't flood --log-level debug output. Independent of
// maxToolOutputBytes (internal/agent/tools.go), which caps what the MODEL
// sees, not what an operator's log line shows.
const debugToolResultPreviewBytes = 500

// debugToolResultPreview renders response as a size-capped preview for the
// agent.tool_result debug log, mirroring truncateToolOutput's
// "... [truncated N bytes]" marker (internal/agent/tools.go). error/
// exit_code are already their own log fields (see executeBudgetedTool), so
// they're excluded here to avoid duplicating them in the preview.
func debugToolResultPreview(response map[string]any) string {
	preview := make(map[string]any, len(response))

	for k, v := range response {
		if k == "error" || k == "exit_code" {
			continue
		}

		preview[k] = v
	}

	b, err := json.Marshal(preview)
	if err != nil {
		return fmt.Sprintf("<unrenderable: %v>", err)
	}

	s := string(b)
	if len(s) <= debugToolResultPreviewBytes {
		return s
	}

	return s[:debugToolResultPreviewBytes] + fmt.Sprintf("... [truncated %d bytes]", len(s)-debugToolResultPreviewBytes)
}
