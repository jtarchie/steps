package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/store"
)

// maxRecordedResultBytes caps how much of a single tool result is persisted in
// a transcript. Results are already bounded by maxToolOutputBytes for what the
// model sees; this tighter cap keeps a transcript a readable record of the
// exchange rather than a second copy of every tool's output.
const maxRecordedResultBytes = 4096

// transcriptEvent is one entry in an agent conversation's persisted
// transcript: the model's visible text for a turn, a tool call it made, the
// result that came back, or a sub-agent delegation carrying the child's own
// nested events. Unlike the trajectory in nodes.result (tool calls only,
// bounded for downstream consumers), the transcript is the full exchange —
// it lives in its own node_transcripts row precisely so nodes.result stays
// small for the readers that load it on every run.
type transcriptEvent struct {
	Type    string            `json:"type"` // "text" | "call" | "result" | "subagent"
	Text    string            `json:"text,omitempty"`
	Name    string            `json:"name,omitempty"`
	Args    map[string]any    `json:"args,omitempty"`
	Content string            `json:"content,omitempty"`
	Agent   string            `json:"agent,omitempty"`
	Request string            `json:"request,omitempty"`
	Events  []transcriptEvent `json:"events,omitempty"`
}

// transcriptRecorder accumulates a single conversation's events in order.
// runAgentConversation owns one per conversation and threads it through
// toolEnv so a sub-agent tool can attach its child's nested transcript. All
// methods are nil-safe: a toolImpl invoked outside a conversation (tests,
// direct calls) carries no recorder.
type transcriptRecorder struct {
	events []transcriptEvent
}

// text records the model's visible text for a turn, including text that
// accompanies tool calls mid-conversation — that running commentary is
// exactly what the bounded trajectory drops.
func (r *transcriptRecorder) text(text string) {
	if r == nil || text == "" {
		return
	}

	r.events = append(r.events, transcriptEvent{Type: "text", Text: text})
}

// call records one model-authored tool call, with over-long argument values
// elided the same way the trajectory elides them (truncateArgs).
func (r *transcriptRecorder) call(name string, args map[string]any) {
	if r == nil {
		return
	}

	r.events = append(r.events, transcriptEvent{Type: "call", Name: name, Args: truncateArgs(args)})
}

// results records the tool results a turn produced, in order.
func (r *transcriptRecorder) results(parts []*genai.Part) {
	if r == nil {
		return
	}

	for _, part := range parts {
		if part.FunctionResponse == nil {
			continue
		}

		r.events = append(r.events, transcriptEvent{
			Type:    "result",
			Name:    part.FunctionResponse.Name,
			Content: renderResultContent(part.FunctionResponse.Response),
		})
	}
}

// subagent records one delegation: the parent's request and the child
// conversation's own events, nested. Called by preparedSubAgent.run with the
// PARENT's recorder, for failed children too — a child that died mid-task is
// the one whose trace is needed most.
func (r *transcriptRecorder) subagent(agent, request string, events []transcriptEvent) {
	if r == nil {
		return
	}

	r.events = append(r.events, transcriptEvent{Type: "subagent", Agent: agent, Request: request, Events: events})
}

// renderResultContent flattens a tool's FunctionResponse map to a bounded
// string for the transcript.
//
// The string values are capped BEFORE marshaling, not after. A read_file
// result carries up to maxReadFileBytes (100,000) and this keeps 4,096 of it,
// so marshaling first meant JSON-escaping ~100KB on every tool result of every
// turn purely to throw almost all of it away — work that scaled with turns ×
// concurrent cells while the output never exceeded the cap. Capping first
// bounds what the encoder ever touches.
//
// truncateToolOutputLimit is the package's existing "cap and say so" helper,
// used here rather than a third spelling of the same marker.
func renderResultContent(response map[string]any) string {
	bounded := make(map[string]any, len(response))

	for key, value := range response {
		if text, ok := value.(string); ok {
			bounded[key] = truncateToolOutputLimit(text, maxRecordedResultBytes)

			continue
		}

		bounded[key] = value
	}

	data, err := json.Marshal(bounded)
	if err != nil {
		return fmt.Sprintf("%v", bounded)
	}

	// A non-string value (a big structured MCP result) can still overshoot, so
	// the whole rendering is capped too — now on something already close to
	// the bound rather than on an unbounded blob.
	return truncateToolOutputLimit(string(data), maxRecordedResultBytes)
}

// saveAgentTranscript persists a conversation's transcript under the step's
// node hash — for every outcome, success or failure, since a failed step's
// transcript is the one a human reconstructs from. Best-effort like
// recordAgentFailure, and on a detached context for the same reason: an
// auxiliary record must neither mask the step's own outcome nor be dropped
// because the step was aborted.
func saveAgentTranscript(ctx context.Context, st *store.Store, hash, jobName string, res conversationResult) {
	if len(res.transcript) == 0 {
		return
	}

	data, err := json.Marshal(res.transcript)
	if err != nil {
		slog.Warn("agent.transcript_marshal", "job", jobName, "hash", hash, "error", err)

		return
	}

	err = st.SaveNodeTranscript(context.WithoutCancel(ctx), hash, jobName, string(data))
	if err != nil {
		slog.Warn("agent.transcript_save", "job", jobName, "hash", hash, "error", err)
	}
}
