package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"google.golang.org/genai"
)

// maxConcurrentReads bounds how many read-only calls from one turn run at once.
const maxConcurrentReads = 4

// toolResponseParts executes each requested tool and packages the results as
// FunctionResponse parts to feed back on the next turn. No call's failure
// (a nonzero exit_code, or an {"error": ...} result) stops any other call in
// the turn, or the conversation — every failure is just data the model sees
// on its next turn and can react to. maxCalls/callCounts enforce each
// budgeted tool's max_calls: — see executeBudgetedTool.
//
// A run of consecutive calls whose tool is in readOnly executes concurrently, up to maxConcurrentReads at a time; any other call is a barrier that starts after everything before it finished and finishes before anything after it starts. Parts always come back in the model's call order.
func toolResponseParts(ctx context.Context, calls []*genai.FunctionCall, env toolEnv, registry map[string]toolImpl, readOnly map[string]bool, maxCalls, callCounts map[string]int) []*genai.Part {
	responses := make([]map[string]any, len(calls))

	for start := 0; start < len(calls); {
		end := start + 1
		for readOnly[calls[start].Name] && end < len(calls) && readOnly[calls[end].Name] {
			end++
		}

		// Admission is decided here, serially and in call order, because callCounts is a plain map and a budget must reject the same call it would in a serial loop.
		rejections := make([]map[string]any, end-start)
		for i := start; i < end; i++ {
			rejections[i-start] = admitTool(calls[i], maxCalls, callCounts)
		}

		runGroup(ctx, calls[start:end], env, registry, rejections, responses[start:end])

		start = end
	}

	parts := make([]*genai.Part, 0, len(calls))

	for i, call := range calls {
		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: responses[i]},
		})
	}

	return parts
}

// runGroup executes calls, writing each result to the same index of out. A group of one runs inline, exactly as the serial loop did; a larger group is read-only calls only, so a panic in one is recovered into that call's error result rather than taking the daemon down.
func runGroup(ctx context.Context, calls []*genai.FunctionCall, env toolEnv, registry map[string]toolImpl, rejections, out []map[string]any) {
	if len(calls) == 1 {
		out[0] = runBudgetedTool(ctx, calls[0], env, registry, rejections[0])

		return
	}

	var (
		wg   sync.WaitGroup
		slot = make(chan struct{}, maxConcurrentReads)
	)

	for i, call := range calls {
		slot <- struct{}{}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-slot }()
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "agent.tool_panic", "tool", call.Name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))

					out[i] = errorResult(fmt.Sprintf("%s: panic: %v", call.Name, p))
				}
			}()

			out[i] = runBudgetedTool(ctx, call, env, registry, rejections[i])
		}()
	}

	wg.Wait()
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
	return runBudgetedTool(ctx, call, env, registry, admitTool(call, maxCalls, callCounts))
}

// admitTool counts call against its tool's budget, returning the rejection to hand the model once the budget is spent and nil when the call may run. It touches callCounts, so it is never called from a goroutine.
func admitTool(call *genai.FunctionCall, maxCalls, callCounts map[string]int) map[string]any {
	if budget, ok := maxCalls[call.Name]; ok && callCounts[call.Name] >= budget {
		return errorResult(fmt.Sprintf("%s: call budget (%d) exhausted for this attempt", call.Name, budget))
	}

	callCounts[call.Name]++

	return nil
}

// runBudgetedTool is executeBudgetedTool after admission: rejection, when non-nil, is the result and the tool never runs.
func runBudgetedTool(ctx context.Context, call *genai.FunctionCall, env toolEnv, registry map[string]toolImpl, rejection map[string]any) map[string]any {
	// Which run/job/step/depth this call belongs to — the identity every
	// nested call (sub-agent, MCP, custom tool) shares with its enclosing
	// conversation, read from the recorder rather than threaded as
	// parameters. Zero-valued outside a conversation (tests, direct calls).
	live := env.transcript.liveIdentity()

	slog.DebugContext(ctx, "agent.tool_call", "tool", call.Name, "id", call.ID,
		"run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex, "depth", live.depth,
		"args", call.Args)

	start := time.Now()

	ctx, span := startToolSpan(ctx, call.Name, call.ID, call.Args)

	response := rejection
	if response == nil {
		response = executeAgentTool(ctx, call, env, registry)
	}

	slog.DebugContext(ctx, "agent.tool_result", "tool", call.Name, "id", call.ID,
		"run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex, "depth", live.depth,
		"duration", time.Since(start), "error", response["error"], "exit_code", response["exit_code"],
		"result", lazyToolResultPreview{response: response})

	span.end(ctx, response)

	return response
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
