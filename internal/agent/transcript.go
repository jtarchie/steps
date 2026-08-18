package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// maxRecordedResultBytes caps how much of a single tool result is persisted in
// a transcript. Results are already bounded by maxToolOutputBytes for what the
// model sees; this cap keeps a transcript from becoming a second full copy of
// every tool's output while staying close enough to what the model actually
// read (16KB vs the 32KB inline cap) that a transcript reader is rarely
// looking at a truncation marker where the decisive evidence was.
const maxRecordedResultBytes = 16_384

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
//
// It is also where a live view is fed from: each recorded event is
// simultaneously published to whatever run-event bus the context carries
// (see internal/events). One recorder feeding both means a conversation
// watched live and the same conversation read back afterwards cannot
// disagree about what happened.
type transcriptRecorder struct {
	events []transcriptEvent
	// live carries the bus plus the identity every published event needs.
	// Zero value publishes nowhere, which is what a test or a terminal run
	// gets.
	live liveContext
}

// liveContext is what the recorder needs in order to publish an event that
// means something to a reader: which run, job, and step the conversation
// belongs to.
//
// It holds the BUS, not the context that carried it. A context stored in a
// struct outlives the call it belongs to and quietly carries a cancellation
// nobody expects; the bus is the only thing actually needed here, and
// resolving it once at construction is both cheaper and honest about the
// lifetime.
type liveContext struct {
	bus       *events.Bus
	runID     string
	job       string
	stepIndex int
	stepName  string
	// depth is how deep in the sub-agent tree this conversation sits. A
	// child's events are published too — a delegation that takes a minute is
	// the thing a watcher most wants to see progressing — and depth is how a
	// reader tells them from the parent's own.
	depth int
}

// liveIdentity returns r's live context, or the zero value when r is nil —
// what a toolImpl invoked outside a conversation (a test, a direct call)
// sees. Lets a log call site read job/step/run identity without its own nil
// check on the recorder.
func (r *transcriptRecorder) liveIdentity() liveContext {
	if r == nil {
		return liveContext{}
	}

	return r.live
}

// publish sends one recorded event to the bus, if there is one.
func (r *transcriptRecorder) publish(eventType, text, name, detail string) {
	if r == nil || r.live.bus == nil {
		return
	}

	r.live.bus.Publish(events.Event{
		Type:      eventType,
		RunID:     r.live.runID,
		Job:       r.live.job,
		StepIndex: r.live.stepIndex,
		StepName:  r.live.stepName,
		StepKind:  "agent",
		Text:      text,
		Name:      name,
		Detail:    detail,
		// Depth rides in Status rather than growing the event for one
		// consumer: a reader only ever asks "is this nested", and the field
		// is unused for agent traffic otherwise.
		Status: depthLabel(r.live.depth),
	})
}

// depthLabel renders sub-agent nesting depth, empty at the top level so the
// common case carries nothing.
func depthLabel(depth int) string {
	if depth <= 0 {
		return ""
	}

	return fmt.Sprintf("depth:%d", depth)
}

// text records the model's visible text for a turn, including text that
// accompanies tool calls mid-conversation — that running commentary is
// exactly what the bounded trajectory drops.
func (r *transcriptRecorder) text(text string) {
	if r == nil || text == "" {
		return
	}

	r.events = append(r.events, transcriptEvent{Type: "text", Text: text})
	r.publish(events.TypeAgentText, text, "", "")
}

// call records one model-authored tool call, with over-long argument values
// elided the same way the trajectory elides them (truncateArgs).
func (r *transcriptRecorder) call(name string, args map[string]any) {
	if r == nil {
		return
	}

	bounded := truncateArgs(args)
	r.events = append(r.events, transcriptEvent{Type: "call", Name: name, Args: bounded})
	r.publish(events.TypeAgentCall, "", name, renderArgs(bounded))
}

// renderArgs renders a call's arguments for the live stream. The stored
// event keeps the map; the wire wants one string.
func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	data, err := json.Marshal(args)
	if err != nil {
		return ""
	}

	return truncateToolOutputLimit(string(data), maxRecordedResultBytes)
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

		content := renderResultContent(part.FunctionResponse.Response)
		r.events = append(r.events, transcriptEvent{
			Type:    "result",
			Name:    part.FunctionResponse.Name,
			Content: content,
		})
		r.publish(events.TypeAgentResult, "", part.FunctionResponse.Name, content)
	}
}

// subagent records one delegation: the parent's request and the child
// conversation's own events, nested. Called by preparedSubAgent.run with the
// PARENT's recorder, for failed children too — a child that died mid-task is
// the one whose trace is needed most.
func (r *transcriptRecorder) subagent(agent, request string, nested []transcriptEvent) {
	if r == nil {
		return
	}

	r.events = append(r.events, transcriptEvent{Type: "subagent", Agent: agent, Request: request, Events: nested})
	r.publish(events.TypeAgentSubagent, request, agent, "")
}

// childRecorder returns a recorder for a delegated conversation: its own
// event list, publishing to the same bus one level deeper. This is what makes
// a sub-agent's work visible while it happens rather than only in the parent's
// summary of it afterwards.
//
// It keeps the PARENT's step identity. A consumer groups events by the step
// they belong to, and a child's turns belong to the step that delegated them;
// republishing them under the child agent's name detaches them from that
// step, which under a concurrent fan-out lands them on whichever sibling cell
// happened to be running. Depth marks the nesting, and the delegation event
// itself already names the agent.
func (r *transcriptRecorder) childRecorder() *transcriptRecorder {
	if r == nil {
		return &transcriptRecorder{}
	}

	child := &transcriptRecorder{live: r.live}
	child.live.depth = r.live.depth + 1

	return child
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

	err = st.SaveNodeTranscript(context.WithoutCancel(ctx), hash, string(data))
	if err != nil {
		slog.Warn("agent.transcript_save", "job", jobName, "hash", hash, "error", err)
	}
}
