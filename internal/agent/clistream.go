package agent

// Reading a coding-agent CLI's transcript back into the shapes this package
// already records.
//
// The CLI emits one JSON object per line while it works. steps parses that
// stream for exactly three things it cannot get any other way: the final
// response text, how many turns it took, and the tool calls it made — the
// trajectory that assert.tool_calls checks and `steps runs` displays.
//
// The event schema belongs to the CLI, not to steps, so parsing is
// deliberately tolerant: an unrecognized event type, an unexpected content
// block, or a malformed line is logged and skipped rather than failing a step
// that may have completed its work perfectly well. The one thing that is NOT
// tolerated is the absence of the terminal result event, since that is the
// difference between "the CLI finished" and "the CLI died mid-sentence".

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// cliStreamMaxLine bounds one event. A single assistant message carrying a
// large tool argument (a whole file to write) is normal, so this is generous;
// it exists to stop a runaway line from consuming memory without limit.
const cliStreamMaxLine = 8 << 20 // 8 MiB

// cliRunResult is one CLI invocation's transcript, reduced.
type cliRunResult struct {
	text       string
	turns      int
	trajectory []recordedToolCall
	// inputTokens/outputTokens are what the CLI reported spending, folded
	// into the step's usage so a job-level budget: still counts a CLI agent.
	inputTokens  int
	outputTokens int
	// cachedTokens is how much of the prompt the provider served from cache.
	// Folded into inputTokens as well (a cached token is still an input token
	// a budget must count), and kept separately because the two answer
	// different questions: what a step spent, and how much of that was cheap.
	cachedTokens int
	// costUSD is the CLI's own figure for what the run cost.
	costUSD float64
	// streamed is every assistant turn's usage added up as it arrived. It
	// duplicates the result event's figures on a run that finished, and it is
	// the ONLY account of one that did not: a child that died mid-conversation
	// emits no result event, so costUSD and the usage fields above are all
	// zero however much it actually spent. See estimateCLICost.
	streamed cliUsage
	// isError is the CLI's own verdict on its run — it exited having failed
	// at the task, as distinct from having crashed (which shows up as an exit
	// status instead).
	isError bool
	// errSubtype is the CLI's machine-readable reason when isError, e.g. a
	// turn limit. Empty otherwise.
	errSubtype string
	// errMessage is the CLI's own prose reason when it reported one, which
	// makes a better step failure than the subtype alone.
	errMessage string
	// sawResult reports whether the terminal result event arrived at all.
	// False means the stream was truncated, which the driver treats as a
	// failed invocation rather than an empty answer.
	sawResult bool
	// sawInit reports whether the system/init event arrived — the one
	// attestation reads (see cliattest.go). Distinct from sawResult: a stream
	// that produced neither is a crash ("exited without reporting a result"),
	// and only a result WITHOUT an init is an attestation failure — the
	// precedence a killed-before-anything child needs to be diagnosed
	// correctly.
	sawInit bool
	// initTools is the tool list the child's own init event reported, unsorted
	// and exactly as received.
	initTools []string
}

// cliEvent is the subset of a stream event this package reads. Everything
// else in the CLI's schema is ignored by construction — adding a field here
// is how you start depending on it.
type cliEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []cliContentBlock `json:"content"`
		// Usage on an ASSISTANT event is that turn's own bill, reported as the
		// turn happens rather than at the end. It is the only account of a
		// crashed attempt's spend there is: total_cost_usd rides the terminal
		// result event, which a child that died never emits.
		Usage cliUsage `json:"usage"`
	} `json:"message"`
	Result   string   `json:"result"`
	NumTurns int      `json:"num_turns"`
	IsError  bool     `json:"is_error"`
	Errors   []string `json:"errors"`
	// Tools is the session's tool list, carried on the system/init event —
	// the attestation surface (see cliattest.go). Absent from every other
	// event type.
	Tools []string `json:"tools"`
	// TotalCostUSD is what the CLI says the run cost. The only provider path
	// steps has that reports a dollar figure at all — the HTTP ones report
	// tokens and leave pricing to whoever knows the rate card.
	TotalCostUSD float64  `json:"total_cost_usd"`
	Usage        cliUsage `json:"usage"`
}

// cliUsage is one bill in the CLI's own units. It appears twice in the schema
// and means the same thing both times: on the terminal result event it is the
// whole run's, and on an assistant event it is that turn's.
type cliUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Cached prompt tokens are counted as input, and counting them is not
	// optional: a cached conversation reports nearly all of its prompt
	// under these two, so reading input_tokens alone under-reports spend
	// by orders of magnitude (9 vs 21560 in one observed run) and leaves a
	// job budget: unable to trip.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// prompt is everything the CLI charged for input on this bill.
func (u cliUsage) prompt() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// cached is the share of that input the provider did not have to read again.
// Both fields count: a creation write is what makes the next run's read free,
// and reporting only the reads calls the first run of a cached conversation
// 0% cached.
func (u cliUsage) cached() int {
	return u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// promptTokens is everything the CLI charged for input on this run.
func (e cliEvent) promptTokens() int { return e.Usage.prompt() }

// cachedTokens is the share of that input the provider did not have to read
// again.
func (e cliEvent) cachedTokens() int { return e.Usage.cached() }

// cliContentBlock is one block of an assistant or user message: the tool_use
// blocks are the calls, the tool_result blocks are their outcomes.
type cliContentBlock struct {
	Type string `json:"type"`
	// Text is the model's own commentary on an assistant turn — what it said
	// while working, as distinct from what it called. Read only since the
	// transcript recorder existed to receive it; before that the field was
	// absent and every word a CLI agent wrote mid-conversation was dropped.
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// parseCLIStream reads a CLI's line-delimited JSON transcript.
//
// It used to return an error only for a failure to READ the stream; it now
// also returns errCLIToolSurface (see cliattest.go) the instant a
// system/init event disagrees with expected — the bridged grant — so the
// call returns EARLY, before the rest of the stream (any further tool calls)
// is even read, rather than merely being reported after the fact. A stream
// that parsed cleanly but never ended is still reported through sawResult, so
// the caller can combine it with the process's exit status — the pair that
// distinguishes "crashed" from "finished badly".
//
// expected is the bridged tool set an attempt was granted (cliToolPermissions).
// A literal nil (as opposed to a non-nil, possibly-empty slice) means "no
// attestation to perform" — every production caller passes cliToolPermissions'
// result, which is never nil, even for a grant of nothing; most existing
// tests pass nil because they predate attestation entirely.
// The recorder may be nil — a caller that only wants the reduced result, and
// every test that predates the transcript, passes one.
func parseCLIStream(reader io.Reader, rec *transcriptRecorder, expected []string) (cliRunResult, error) {
	var result cliRunResult

	attesting := expected != nil

	// Tool calls are indexed by the id the CLI assigns them, so a result
	// block arriving several events later can mark the right one.
	index := map[string]int{}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), cliStreamMaxLine)

	for scanner.Scan() {
		err := parseCLILine(scanner.Bytes(), &result, rec, index, expected, attesting)
		if err != nil {
			return result, err
		}
	}

	err := scanner.Err()
	if err != nil {
		return result, fmt.Errorf("reading cli stream: %w", err)
	}

	// The precedence fix: a result with no init IS an attestation failure —
	// the child finished without ever proving its tool surface — but a
	// stream that produced NEITHER stays "exited without reporting a
	// result" (execCLI's own fallback), since a child that died before
	// saying anything was never in a position to misreport its tools either.
	if attesting && result.sawResult && !result.sawInit {
		return result, fmt.Errorf("%w: the cli finished without ever reporting a system/init event listing its tools — "+
			"check that the cli's minimum supported version reports `tools` on it", errCLIToolSurface)
	}

	return result, nil
}

// parseCLILine unmarshals and dispatches one line of the stream, extracted
// from parseCLIStream to keep that function's complexity within the linter's
// budget. A blank or unparsable line is skipped, not an error — the schema
// belongs to the CLI, so tolerance is the point (see the file header).
func parseCLILine(
	line []byte, result *cliRunResult, rec *transcriptRecorder, index map[string]int, expected []string, attesting bool,
) error {
	if len(line) == 0 {
		return nil
	}

	var event cliEvent

	err := json.Unmarshal(line, &event)
	if err != nil {
		slog.Debug("agent.cli.stream.skip", "reason", "unparsable line", "error", err)

		return nil
	}

	switch event.Type {
	case "assistant":
		recordCLIToolCalls(result, index, event)
		recordCLITurn(rec, event)
		addCLIUsage(&result.streamed, event.Message.Usage)
	case "user":
		markCLIToolResults(result, index, event)
		recordCLIResults(rec, result.trajectory, index, event)
	case "system":
		return recordCLIInit(result, event, expected, attesting)
	case "result":
		recordCLIResult(result, event)
	default:
		slog.Debug("agent.cli.stream.skip", "type", event.Type)
	}

	return nil
}

// recordCLIInit handles one system event: recorded only when it is the
// system/init event attestation reads, and checked against expected the
// instant it is parsed when attesting — extracted from parseCLIStream's own
// switch to keep that function's complexity within the linter's budget.
func recordCLIInit(result *cliRunResult, event cliEvent, expected []string, attesting bool) error {
	if event.Subtype != "init" {
		slog.Debug("agent.cli.stream.skip", "type", event.Type, "subtype", event.Subtype)

		return nil
	}

	result.sawInit = true
	result.initTools = event.Tools

	if !attesting {
		return nil
	}

	// Checked the instant this line is parsed, not after the stream ends: the
	// point is stopping a child this process no longer trusts before it makes
	// another bridged call.
	err := checkCLIToolSurface(event.Tools, expected)
	if err != nil {
		return err
	}

	return nil
}

// recordCLIResult handles the terminal result event — extracted from
// parseCLIStream's own switch for the same complexity-budget reason as
// recordCLIInit.
func recordCLIResult(result *cliRunResult, event cliEvent) {
	result.sawResult = true
	result.text = event.Result
	result.turns = event.NumTurns
	result.isError = event.IsError
	result.errSubtype = event.Subtype
	result.inputTokens = event.promptTokens()
	result.outputTokens = event.Usage.OutputTokens
	result.cachedTokens = event.cachedTokens()
	result.costUSD = event.TotalCostUSD

	if len(event.Errors) > 0 {
		result.errMessage = event.Errors[0]
	}
}

// addCLIUsage folds one turn's bill into a running total. Each assistant
// event reports its OWN call's usage, so a multi-turn run's cost is the sum:
// every turn re-reads the prompt and is billed for it again, cache reads
// included.
func addCLIUsage(total *cliUsage, turn cliUsage) {
	total.InputTokens += turn.InputTokens
	total.OutputTokens += turn.OutputTokens
	total.CacheCreationInputTokens += turn.CacheCreationInputTokens
	total.CacheReadInputTokens += turn.CacheReadInputTokens
}

// recordCLIToolCalls appends this assistant turn's tool calls to the
// trajectory, DE-NAMESPACED — `mcp__steps__read_file` records as `read_file`,
// identical to what a hosted step's trajectory shows for the same call. This
// is a deliberate reversal of the old "names are recorded exactly as the CLI
// reported them" rule: `mcp__steps__` is OUR OWN server's namespace (see
// clibridge.go), so removing it is de-namespacing what steps itself put
// there, not translating an observation into something it wasn't. It also
// makes `assert.tool_calls: [{name: read_file}]` mean the same thing on a
// CLI agent as on a hosted one, which is the issue #100 goal.
//
// A name this build does not recognize as bridged (`Bash`, `Task`, anything
// the CLI's own natives could still report despite `--tools ""`) is kept
// VERBATIM rather than guessed at — which doubles as a second, human-readable
// surplus-tool signal alongside the init-event attestation (cliattest.go): a
// stray `Bash` entry in a recorded trajectory is exactly what a fence failure
// looks like to a reader of `steps runs --steps`.
func recordCLIToolCalls(result *cliRunResult, index map[string]int, event cliEvent) {
	for _, block := range event.Message.Content {
		if block.Type != "tool_use" || block.Name == "" {
			continue
		}

		// Bounded the same way the hosted path bounds its own trajectory
		// (truncateArgs). Stored raw, one Write call carried a whole file
		// into the node's result — the CLI path was the only one paying that
		// on disk, which footprint_test.go exists to keep honest.
		args := truncateArgs(block.Input)
		if args == nil {
			args = map[string]any{}
		}

		// Optimistically ok: a call with no matching result block (the CLI
		// was interrupted before it reported one) reads as having run, which
		// is the safer direction for a record of what touched the workspace.
		result.trajectory = append(result.trajectory, recordedToolCall{name: debridgedToolName(block.Name), args: args, ok: true})

		if block.ID != "" {
			index[block.ID] = len(result.trajectory) - 1
		}
	}
}

// recordCLITurn hands one assistant turn to the transcript: what the model
// said, then what it called, in the order the blocks arrived.
//
// Recorded from the STREAM rather than from the bridge, including for bridged
// tools that the parent itself executes. The stream sees both kinds and is
// authoritative for order (the same rule mergeCLITrajectory follows), so
// recording the bridge's view as well would show every bridged call twice
// and race the stdout reader for the position it appears at. De-namespaced
// exactly as recordCLIToolCalls is, for the same reason.
func recordCLITurn(rec *transcriptRecorder, event cliEvent) {
	for _, block := range event.Message.Content {
		switch block.Type {
		case "text":
			rec.text(block.Text)
		case "tool_use":
			if block.Name != "" {
				rec.call(debridgedToolName(block.Name), block.Input)
			}
		}
	}
}

// recordCLIResults hands a user turn's tool results to the transcript,
// resolving each one's tool NAME through the same id index markCLIToolResults
// uses — a tool_result block carries the call's id and never its name.
func recordCLIResults(rec *transcriptRecorder, trajectory []recordedToolCall, index map[string]int, event cliEvent) {
	if rec == nil {
		return
	}

	for _, block := range event.Message.Content {
		if block.Type != "tool_result" {
			continue
		}

		at, ok := index[block.ToolUseID]
		if !ok || at >= len(trajectory) {
			continue
		}

		// Bounded here rather than in the recorder: a stream result is the
		// one arrival that carries no cap of its own, where the hosted path's
		// renderResultContent has already applied one (see recorder.result).
		rec.result(trajectory[at].name, truncateToolOutputLimit(cliResultText(block.Content), maxRecordedResultBytes))
	}
}

// cliResultText flattens a tool_result's content, which the CLI sends either
// as a bare string or as the block array the Messages API uses.
func cliResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string

	err := json.Unmarshal(raw, &text)
	if err == nil {
		return text
	}

	var blocks []cliContentBlock

	err = json.Unmarshal(raw, &blocks)
	if err != nil {
		return string(raw)
	}

	var out strings.Builder

	for _, block := range blocks {
		out.WriteString(block.Text)
	}

	return out.String()
}

// markCLIToolResults backfills ok from the tool_result blocks in a user turn.
func markCLIToolResults(result *cliRunResult, index map[string]int, event cliEvent) {
	for _, block := range event.Message.Content {
		if block.Type != "tool_result" {
			continue
		}

		at, ok := index[block.ToolUseID]
		if !ok {
			continue
		}

		result.trajectory[at].ok = !block.IsError
	}
}
