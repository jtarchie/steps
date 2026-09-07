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
// transcript: the system prompt, a user/task message, the model's visible
// text for a turn, a tool call (model-issued or the synthetic
// upstream/context_paths exchanges built into the opening request), the
// result that came back, or a sub-agent delegation carrying the child's own
// nested events. Unlike the trajectory in nodes.result (tool calls only,
// bounded for downstream consumers), the transcript is the full exchange —
// it lives in its own node_transcripts row precisely so nodes.result stays
// small for the readers that load it on every run.
type transcriptEvent struct {
	Type    string            `json:"type"` // "system" | "user" | "text" | "call" | "result" | "subagent"
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
	// stepID is the step's place in the run's display tree, so a turn hangs
	// under the step that produced it rather than being matched back by
	// (index, name) — a pair a try: shares with the step inside it.
	stepID int64
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
		StepID:    r.live.stepID,
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

// system records the system prompt a conversation started with. Called once,
// from buildAgentRequest's fresh branch (never on a failover resume, which
// reuses the same conversation rather than starting a new one) — the same
// placement user() uses, for the same reason.
//
// Bounded the same as a tool result: a system_file: can be arbitrarily large,
// and on the CLI path this is also where the whole rendered prompt lands
// (recordCLIOpening), which folds in every upstream/context block with no
// cap of its own (renderCLIPrompt). Capping here, once, protects every
// caller instead of relying on each one to remember to.
func (r *transcriptRecorder) system(text string) {
	if r == nil || text == "" {
		return
	}

	text = truncateToolOutputLimit(text, maxRecordedResultBytes)

	r.events = append(r.events, transcriptEvent{Type: "system", Text: text})
	r.publish(events.TypeAgentSystem, text, "", "")
}

// user records a user/task message: the opening message a conversation
// starts with, or a later message: entry sent mid-conversation via advance.
// Both call sites fire exactly once per real message — see buildAgentRequest
// and advance in conversation.go.
//
// Bounded like system(): the CLI path's recordCLIOpening/
// recordCLIMessageDelivery record the fully-rendered prompt here, which can
// carry an unbounded context_paths:/upstream block folded in by
// renderCLIPrompt.
func (r *transcriptRecorder) user(text string) {
	if r == nil || text == "" {
		return
	}

	text = truncateToolOutputLimit(text, maxRecordedResultBytes)

	r.events = append(r.events, transcriptEvent{Type: "user", Text: text})
	r.publish(events.TypeAgentUser, text, "", "")
}

// pendingIndex reports how many events are recorded so far — for a caller
// that must reserve a position before doing work whose OUTCOME decides
// whether an earlier turn gets recorded at all. See insertUserAt: cli.go's
// recordCLIMessageDelivery only learns a resumed prompt was delivered once
// the child's whole reply to it has already streamed in and been recorded,
// so it cannot simply append the prompt afterward without landing it below
// the answer it prompted.
func (r *transcriptRecorder) pendingIndex() int {
	if r == nil {
		return 0
	}

	return len(r.events)
}

// insertUserAt records text as a user event at position at in the PERSISTED
// transcript, ahead of whatever was appended at or after at in the meantime,
// rather than at the end — the counterpart to user() for a caller that
// learns a turn belongs earlier only after later turns already landed.
//
// Only the persisted slice is reordered; publish() still fires now, in call
// order, same as ever — a live viewer sees the child's reply stream in
// before delivery of the prompt it answered is confirmed, which is inherent
// to how CLI delivery is only known to have succeeded once the reply is
// already back, not a defect this can retroactively undo for a connection
// already watching.
func (r *transcriptRecorder) insertUserAt(at int, text string) {
	if r == nil || text == "" {
		return
	}

	text = truncateToolOutputLimit(text, maxRecordedResultBytes)

	if at < 0 || at > len(r.events) {
		at = len(r.events)
	}

	r.events = append(r.events, transcriptEvent{})
	copy(r.events[at+1:], r.events[at:])
	r.events[at] = transcriptEvent{Type: "user", Text: text}

	r.publish(events.TypeAgentUser, text, "", "")
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

		r.result(part.FunctionResponse.Name, renderResultContent(part.FunctionResponse.Response))
	}
}

// result records ONE tool result already rendered to text AND already bounded
// by whoever rendered it — renderResultContent below on the hosted path,
// recordCLIResults on the CLI one.
//
// The seam between the two paths: the hosted loop arrives with genai parts and
// flattens them above, while a CLI transcript arrives as a name and a content
// string it read off the child's stream (see clistream.go). Both land here, so
// there is one place a result becomes an event and no way for the two to
// drift.
//
// It deliberately does not cap again. truncateToolOutputLimit appends a marker
// naming how much it dropped, so re-capping an already-marked string cuts that
// marker off and replaces it with one reporting the MARKER's own size — a
// 100KB result then claims 25 bytes were truncated.
func (r *transcriptRecorder) result(name, content string) {
	if r == nil || name == "" {
		return
	}

	r.events = append(r.events, transcriptEvent{Type: "result", Name: name, Content: content})
	r.publish(events.TypeAgentResult, "", name, content)
}

// recorded is the conversation captured so far, nil-safe so a caller that
// never had a recorder (a direct invocation, a test) reads an empty
// transcript rather than branching on it.
func (r *transcriptRecorder) recorded() []transcriptEvent {
	if r == nil {
		return nil
	}

	return r.events
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
