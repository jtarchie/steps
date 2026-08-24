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
// It returns an error only for a failure to READ the stream. A stream that
// parsed but never ended is reported through sawResult, so the caller can
// combine it with the process's exit status — which is the pair that actually
// distinguishes "crashed" from "finished badly".
// The recorder may be nil — a caller that only wants the reduced result, and
// every test that predates the transcript, passes one.
func parseCLIStream(reader io.Reader, rec *transcriptRecorder) (cliRunResult, error) {
	var result cliRunResult

	// Tool calls are indexed by the id the CLI assigns them, so a result
	// block arriving several events later can mark the right one.
	index := map[string]int{}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), cliStreamMaxLine)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event cliEvent

		err := json.Unmarshal(line, &event)
		if err != nil {
			slog.Debug("agent.cli.stream.skip", "reason", "unparsable line", "error", err)

			continue
		}

		switch event.Type {
		case "assistant":
			recordCLIToolCalls(&result, index, event)
			recordCLITurn(rec, event)
			addCLIUsage(&result.streamed, event.Message.Usage)
		case "user":
			markCLIToolResults(&result, index, event)
			recordCLIResults(rec, result.trajectory, index, event)
		case "result":
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
		default:
			slog.Debug("agent.cli.stream.skip", "type", event.Type)
		}
	}

	err := scanner.Err()
	if err != nil {
		return result, fmt.Errorf("reading cli stream: %w", err)
	}

	return result, nil
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
// trajectory. Names are recorded exactly as the CLI reports them — `Bash`,
// `Read`, `mcp__steps__verdict` — because that is what actually ran; renaming
// them back to steps' own builtin names would make the record a translation
// rather than an observation.
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
		result.trajectory = append(result.trajectory, recordedToolCall{name: block.Name, args: args, ok: true})

		if block.ID != "" {
			index[block.ID] = len(result.trajectory) - 1
		}
	}
}

// recordCLITurn hands one assistant turn to the transcript: what the model
// said, then what it called, in the order the blocks arrived.
//
// Recorded from the STREAM rather than from the bridge, including for bridged
// mcp__steps__* tools that the parent itself executes. The stream sees both
// kinds and is authoritative for order (the same rule mergeCLITrajectory
// follows), so recording the bridge's view as well would show every bridged
// call twice and race the stdout reader for the position it appears at.
func recordCLITurn(rec *transcriptRecorder, event cliEvent) {
	for _, block := range event.Message.Content {
		switch block.Type {
		case "text":
			rec.text(block.Text)
		case "tool_use":
			if block.Name != "" {
				rec.call(block.Name, block.Input)
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
