package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// Handoff describes how control arrived at a step via a to:/verdicts:
// transition — assembled by internal/pipeline immediately before dispatching
// the routed-to step, and consumed here (never by internal/pipeline itself)
// to build the step's <transition_context> prompt block and/or its
// previous_run tool (see config.HandoffSpec, renderHandoffBlock,
// injectHandoffTool). Only ever passed to RunStep for a step whose handoff:
// enables something; a nil Handoff (the step's first/unrouted execution) is
// the common case and costs nothing.
type Handoff struct {
	// JobName is the enclosing job, for the position line in
	// renderHandoffBlock.
	JobName string
	// FromStep is the routed-from step's name (its task/put/agent value).
	FromStep string
	// RouteKey is the routing key the transition matched: a verdict, or
	// "success"/"failure".
	RouteKey string
	// Note is the sender's verdict note, "" if none was given.
	Note string
	// Visit is this step's upcoming execution count (1-based) in the current
	// run.
	Visit int
	// MaxVisits is the receiving step's own max_visits; 0 means unbounded.
	MaxVisits int
	// StepIndex is the receiving step's position within its plan segment
	// (0-based); PlanLen is that segment's length.
	StepIndex int
	PlanLen   int
	// Previous is the routed-from step's recorded run, packaged for the
	// previous_run tool — nil unless the routing step was an agent.
	Previous *PreviousRun
}

// PreviousRun is the routed-from agent step's recorded run, served on demand
// by the synthesized previous_run tool (see injectHandoffTool) rather than
// pushed into the receiving step's prompt — full fidelity, but only when
// asked for.
type PreviousRun struct {
	Agent      string
	Response   string
	Verdict    string
	Note       string
	Turns      int
	Trajectory []ToolCall
}

// ToolCall is one call from a PreviousRun's trajectory: the tool name and
// the model-authored arguments it was called with.
type ToolCall struct {
	Name string
	Args map[string]any
}

// exportTrajectory converts an internal recordedToolCall slice (see
// conversation.go) into the exported ToolCall shape a PreviousRun/
// previous_run tool result serializes.
func exportTrajectory(trajectory []recordedToolCall) []ToolCall {
	out := make([]ToolCall, len(trajectory))
	for i, call := range trajectory {
		out[i] = ToolCall{Name: call.name, Args: call.args}
	}

	return out
}

// renderHandoffBlock formats h as the <transition_context> block appended to
// a routed-to step's prompt (see promptWithHandoff). Delimited with
// HTML-style tags — a convention for marking where injected, machine-
// assembled context starts and stops, distinct from the surrounding prose.
// spillDir is the receiving step's own spill directory (already created by
// prepareAgentStep by the time this is called — see step.go), used to spill
// an oversized note to a file rather than dropping the overflow; "" degrades
// to a plain truncation, matching spillOrTruncate's own degrade behavior.
func renderHandoffBlock(h *Handoff, spillDir string) string {
	var b strings.Builder

	b.WriteString("<transition_context>\n")
	fmt.Fprintf(&b, "entered via: %s (from step %q)\n", h.RouteKey, h.FromStep)

	if h.MaxVisits > 0 {
		fmt.Fprintf(&b, "visit: %d of %d for this step\n", h.Visit, h.MaxVisits)
	} else {
		fmt.Fprintf(&b, "visit: %d (unbounded) for this step\n", h.Visit)
	}

	fmt.Fprintf(&b, "position: step %d of %d in job %q\n", h.StepIndex+1, h.PlanLen, h.JobName)

	if h.Note != "" {
		fmt.Fprintf(&b, "<note from=%q>\n%s\n</note>\n", h.FromStep, sanitizeHandoffNote(spillOrTruncate(h.Note, spillDir)))
	}

	b.WriteString("</transition_context>")

	return b.String()
}

// sanitizeHandoffNote strips any literal "</note>" from upstream
// model-authored text before it's embedded inside the <note> element. A
// verdict's note is text a prior step's model chose — the same trust domain
// as the fix agent's captured failure output — and must not be able to close
// the element early and inject fabricated context after it.
func sanitizeHandoffNote(note string) string {
	return strings.ReplaceAll(note, "</note>", "")
}

// promptWithHandoff appends the routed-entry <transition_context> block to
// prompt when spec enables it. handoff is nil on a step's first/unrouted
// execution (see internal/pipeline's pending carry), in which case no block
// is appended even when spec.Context is set — there is nothing to describe.
// spillDir is threaded through to renderHandoffBlock — see its doc comment.
func promptWithHandoff(prompt string, spec *config.HandoffSpec, handoff *Handoff, spillDir string) string {
	if spec == nil || !spec.Context || handoff == nil {
		return prompt
	}

	return prompt + "\n\n" + renderHandoffBlock(handoff, spillDir)
}

// synthesizedTools is what injectSynthesizedTools produced beyond the tool
// set it mutated in place: the verdict tool's name (see injectVerdictTool),
// "" when the step declares no verdicts; and the rendered run-context recap
// (see buildRecap), "" when there is nothing to show.
type synthesizedTools struct {
	verdictTool string
	recap       string
}

// synthesisInputs is everything the synthesized tools are derived from beyond
// the step itself: the transition context a previous_run tool serves, the
// store seam both halves of the run context need, and the run identity that
// scopes it.
type synthesisInputs struct {
	handoff *Handoff
	store   *store.Store
	// readScopes is what this step can SEE: the run, plus every concurrent
	// block it sits inside, nearest last. See ContextReadScopes.
	readScopes []string
	// writeScope is where what it RECORDS lands — the run, except inside a
	// concurrent branch. See WithContextScope.
	writeScope string
}

// injectSynthesizedTools adds the tools a step's own declarations call for
// onto an already-built tool set: the required verdict tool (step.Verdicts),
// the required write_handoff tool (handoff: {note: true}), the read-only
// previous_run tool (handoff: {tool: true}), the set_context writer
// (context: write), and the read_context reader whenever this run has recorded
// anything at all.
//
// Split out of prepareAgentStep to keep its own cyclomatic complexity down —
// every injection shares one close-on-error handler at the call site.
func injectSynthesizedTools(
	ctx context.Context, cfg *config.Config, step config.Step, in synthesisInputs,
	decls *genai.Tool, registry map[string]toolImpl, required map[string]bool,
) (synthesizedTools, error) {
	verdictTool, err := injectVerdictTool(step.VerdictNames(), step.NoteRequired, decls, registry, required)
	if err != nil {
		return synthesizedTools{}, err
	}

	// Writes go to the write SCOPE, which is the run itself except inside a
	// concurrent branch (see WithContextScope); reads below layer that scope
	// over the run, so a branch sees everything established before the block
	// AND what it has recorded itself.
	//
	// Attributed to the name the step is KNOWN by, so a matrix's recorded facts
	// say which cell established them rather than naming the agent three times.
	err = injectSetContextTool(step, contextWriterFor(in.store, in.writeScope, step.DisplayName()), decls, registry)
	if err != nil {
		return synthesizedTools{}, err
	}

	// The recap is read here rather than at the call site so the read_context
	// tool it implies is injected with every other synthesized tool, in one
	// place, under one error handler.
	recap, entries, err := buildRecap(ctx, in.store, in.readScopes, cfg.ResolveContextFidelity(step))
	if err != nil {
		return synthesizedTools{}, err
	}

	err = injectReadContextTool(entries, decls, registry)
	if err != nil {
		return synthesizedTools{}, err
	}

	// A step may require BOTH a verdict and a handoff note; the required-tool
	// machinery already handles more than one (unsatisfiedRequiredTools forces
	// them one per turn until all are satisfied).
	_, err = injectWriteHandoffTool(step, decls, registry, required)
	if err != nil {
		return synthesizedTools{}, err
	}

	if step.Handoff != nil && step.Handoff.Tool {
		err = injectHandoffTool(in.handoff, decls, registry)
		if err != nil {
			return synthesizedTools{}, err
		}
	}

	return synthesizedTools{verdictTool: verdictTool, recap: recap}, nil
}

// previousRunToolName is the fixed name of the synthesized read-only tool a
// handoff: {tool: true} step is granted. A custom tool of the same name is
// rejected at prepareAgentStep (it would collide with this synthesized one) —
// mirrors verdictToolName's collision treatment.
const previousRunToolName = "previous_run"

// injectHandoffTool appends the synthesized previous_run tool to an agent
// step's already-built tool set. handoff may be nil (first/unrouted
// execution) or have a nil Previous (the routing step wasn't an agent) —
// both are legitimate "nothing to report" cases the tool answers as data,
// not an error. Unlike the verdict tool, previous_run is never required: —
// it's offered, not demanded.
func injectHandoffTool(handoff *Handoff, decls *genai.Tool, registry map[string]toolImpl) error {
	if _, exists := registry[previousRunToolName]; exists {
		return fmt.Errorf("declares handoff: {tool: true} but already defines a tool named %q", previousRunToolName)
	}

	decl := &genai.FunctionDeclaration{
		Name: previousRunToolName,
		Description: "Look up the recorded run of the step that transitioned control to this one: its final response, " +
			"verdict and note (if any), turn count, and tool-call trajectory. Returns \"no previous run\" if this is the " +
			"first execution of this step, or the routing step was not an agent.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"section": {
					Type:        genai.TypeString,
					Enum:        []string{"all", "response", "trajectory"},
					Description: `Which part to return. Defaults to "all".`,
				},
			},
		},
	}

	decls.FunctionDeclarations = append(decls.FunctionDeclarations, decl)
	registry[previousRunToolName] = previousRunToolImpl(handoff)

	return nil
}

// previousRunSections are the valid values of previous_run's optional
// section argument.
var previousRunSections = map[string]bool{"all": true, "response": true, "trajectory": true} //nolint:gochecknoglobals // static, read-only lookup table

// previousRunToolImpl closes over handoff (captured at prepare time) and
// returns its Previous run as data, filtered by the model-requested section.
// Every path returns success data, never a Go error — including "no previous
// run", which is a legitimate answer, not a failure (see the failure-as-data
// contract documented on toolImpl). env.spillDir (the receiving step's own
// spill directory) is threaded into both the response half and the
// trajectory half so an oversized field spills to a file instead of being
// dropped.
func previousRunToolImpl(handoff *Handoff) toolImpl {
	return func(_ context.Context, args map[string]any, env toolEnv) map[string]any {
		if handoff == nil || handoff.Previous == nil {
			return map[string]any{"result": "no previous run: this is the first execution of this step, or the routing step was not an agent"}
		}

		section := stringArg(args, "section")
		if section == "" {
			section = "all"
		}

		if !previousRunSections[section] {
			return map[string]any{"error": fmt.Sprintf("previous_run: unknown section %q (expected all, response, or trajectory)", section)}
		}

		result := map[string]any{}

		if section == "all" || section == "response" {
			addPreviousRunResponse(result, handoff.Previous, env.spillDir)
		}

		if section == "all" || section == "trajectory" {
			result["trajectory"] = previousRunTrajectory(handoff.Previous.Trajectory, env.spillDir)
		}

		return result
	}
}

// addPreviousRunResponse fills in result's response-half fields (agent,
// turns, verdict/note when set, and the response text, spilled to a file
// when oversized) from prev.
func addPreviousRunResponse(result map[string]any, prev *PreviousRun, spillDir string) {
	result["agent"] = prev.Agent
	result["turns"] = prev.Turns

	if prev.Verdict != "" {
		result["verdict"] = prev.Verdict
	}

	if prev.Note != "" {
		result["note"] = spillOrTruncate(prev.Note, spillDir)
	}

	result["response"] = spillOrTruncate(prev.Response, spillDir)
}

// previousRunTrajectory converts a PreviousRun's trajectory into the plain
// map shape previous_run's result serializes, capping each call's arg values
// (see boundedTrajectoryArgs) so a single oversized arg from a prior turn —
// e.g. a write_file call's content — can't flood this tool's own result the
// way an uncapped trajectory would.
func previousRunTrajectory(trajectory []ToolCall, spillDir string) []map[string]any {
	calls := make([]map[string]any, len(trajectory))
	for i, call := range trajectory {
		calls[i] = map[string]any{"tool": call.Name, "args": boundedTrajectoryArgs(call.Args, spillDir)}
	}

	return calls
}

// boundedTrajectoryArgs returns a copy of args with any oversized value
// replaced by a spilled-file pointer message (see spillOrTruncate), leaving
// every value at or under maxToolOutputBytes untouched — same type, same
// shape — so the common case (short args) round-trips exactly as recorded.
// A string value is measured/spilled directly, preserving its raw text in
// the spill file; any other value (number, bool, nested map/slice) is
// measured/spilled via its marshaled JSON form instead, since there's no
// other text representation to spill.
func boundedTrajectoryArgs(args map[string]any, spillDir string) map[string]any {
	bounded := make(map[string]any, len(args))

	for k, v := range args {
		if s, ok := v.(string); ok {
			bounded[k] = spillOrTruncate(s, spillDir)

			continue
		}

		data, err := json.Marshal(v)
		if err != nil || len(data) <= maxToolOutputBytes {
			bounded[k] = v

			continue
		}

		bounded[k] = spillOrTruncate(string(data), spillDir)
	}

	return bounded
}
