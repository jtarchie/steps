// loop.go detects an agent stuck repeating itself and breaks the cycle,
// copying the shape of crush's loop_detection.go: a tool INTERACTION (the
// call plus the result it produced) is hashed, and one identical interaction
// seen too often inside a sliding window of recent turns means the model is
// spinning — re-asking a question whose answer cannot change.
//
// Hashing the result alongside the call is what keeps legitimate repetition
// from tripping the detector: re-reading a file after an edit returns
// different bytes, paging a file returns different ranges, and a verify
// command's output changes as the tree is fixed — each is a different
// interaction. Only call-and-result-both-identical accumulates.
//
// The reaction is two-strike, tuned for a CI pipeline rather than an
// interactive session (where crush merely warns): the first detection
// appends a warning message and lets the conversation continue; a second
// detection — the model repeated the same interaction anyway, since the
// window is not reset — fails the attempt as a task failure (outcome.Fail,
// so hook dispatch classifies it failed, not errored). Without this, a
// stuck agent burns its entire max_turns budget producing nothing a human
// would call progress.
//
// One deliberate deviation from crush: crush requires a full window of
// steps before detecting anything, which would protect only agents whose
// max_turns reaches the window. Here any count over the repeat threshold
// triggers, however short the conversation — an agent with the default 8
// turns deserves the same rescue as one with 200.

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/outcome"
)

const (
	// loopDetectionWindow is how many recent tool-executing turns the
	// detector remembers. Turns older than this stop counting — a model
	// that warned, went and did other work, and legitimately needs the
	// same read again later is forgiven by the slide, not by a reset.
	loopDetectionWindow = 10
	// loopDetectionMaxRepeats is how many copies of one identical
	// interaction (same tool, same args, same result) the window may hold
	// before the model is declared stuck. crush uses the same 10/5 pair;
	// there is no steps-specific reason to tune it differently.
	loopDetectionMaxRepeats = 5
)

// loopDetector is one conversation's repetition tracker, scoped like
// callCounts and trajectory beside it.
type loopDetector struct {
	// turns holds each recent turn's signature counts, oldest first, capped
	// at loopDetectionWindow entries.
	turns []map[string]int
	// names maps a signature back to its tool name for messages. Bounded
	// by the same window in spirit; old entries are harmless (a stale
	// name for a sig that recurs would still be the right tool, since the
	// sig covers name+args+result).
	names map[string]string
	// nudged records whether the warning has already been delivered this
	// attempt — the two-strike state.
	nudged bool
}

func newLoopDetector() *loopDetector {
	return &loopDetector{names: make(map[string]string)}
}

// record adds one turn's tool interactions to the window and reports
// whether any single interaction has now repeated more than
// loopDetectionMaxRepeats times within it, returning the offending tool's
// name. calls and parts are zipped by index — toolResponseParts preserves
// request order.
func (d *loopDetector) record(calls []*genai.FunctionCall, parts []*genai.Part) (string, bool) {
	counts := make(map[string]int, len(calls))

	for i, call := range calls {
		var response map[string]any
		if i < len(parts) && parts[i].FunctionResponse != nil {
			response = parts[i].FunctionResponse.Response
		}

		sig := loopSignature(call.Name, call.Args, response)
		counts[sig]++
		d.names[sig] = call.Name
	}

	d.turns = append(d.turns, counts)
	if len(d.turns) > loopDetectionWindow {
		d.turns = d.turns[len(d.turns)-loopDetectionWindow:]
	}

	totals := make(map[string]int, len(d.names))

	for _, turn := range d.turns {
		for sig, n := range turn {
			totals[sig] += n
			if totals[sig] > loopDetectionMaxRepeats {
				return d.names[sig], true
			}
		}
	}

	return "", false
}

// respond applies the two-strike reaction after one turn's tool results
// have been appended to req: the first detection appends the warning and
// lets the conversation continue (nil error); the second — the model
// repeated the same interaction anyway, since the window is never reset —
// returns the attempt-failing error. Callers return that error verbatim: it
// is already classified as a task failure (outcome.Fail), so hook dispatch
// treats it as failed rather than errored.
func (d *loopDetector) respond(req *model.LLMRequest, calls []*genai.FunctionCall, parts []*genai.Part) error {
	repeated, stuck := d.record(calls, parts)
	if !stuck {
		return nil
	}

	if d.nudged {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(fmt.Errorf("agent stuck in a loop: kept repeating an identical %q call after being warned", repeated))
	}

	d.nudged = true

	req.Contents = append(req.Contents, &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: loopNudge(repeated)}},
	})

	return nil
}

// loopSignature hashes one tool interaction — name, model-authored args,
// and the result the tool returned — into a compact key. json.Marshal's
// deterministic key ordering makes the same logical args/result hash equal
// every time; a sha256 keeps memory bounded when results run to the 32KB
// inline cap.
func loopSignature(name string, args, response map[string]any) string {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		argsJSON = []byte("null") // model-authored args should always marshal; a fallback still keeps signatures comparable
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		responseJSON = []byte("null")
	}

	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(argsJSON)
	h.Write([]byte{0})
	h.Write(responseJSON)

	return hex.EncodeToString(h.Sum(nil))
}

// loopNudge is the warning appended to the conversation on first detection,
// phrased so the model knows what to do instead of repeating the call.
func loopNudge(tool string) string {
	return fmt.Sprintf(
		"You have called %s with identical arguments and received an identical result more than %d times within the last %d turns — you are stuck in a loop. That call's result will not change if you make it again. Do something materially different: use a different tool, different arguments, or a different part of the workspace. If the task is genuinely blocked, reply with a final message explaining what is blocking you.",
		tool, loopDetectionMaxRepeats, loopDetectionWindow,
	)
}
