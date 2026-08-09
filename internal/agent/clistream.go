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
	// isError is the CLI's own verdict on its run — it exited having failed
	// at the task, as distinct from having crashed (which shows up as an exit
	// status instead).
	isError bool
	// errSubtype is the CLI's machine-readable reason when isError, e.g. a
	// turn limit. Empty otherwise.
	errSubtype string
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
	} `json:"message"`
	Result   string `json:"result"`
	NumTurns int    `json:"num_turns"`
	IsError  bool   `json:"is_error"`
	Usage    struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// cliContentBlock is one block of an assistant or user message: the tool_use
// blocks are the calls, the tool_result blocks are their outcomes.
type cliContentBlock struct {
	Type      string          `json:"type"`
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
func parseCLIStream(reader io.Reader) (cliRunResult, error) {
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
		case "user":
			markCLIToolResults(&result, index, event)
		case "result":
			result.sawResult = true
			result.text = event.Result
			result.turns = event.NumTurns
			result.isError = event.IsError
			result.errSubtype = event.Subtype
			result.inputTokens = event.Usage.InputTokens
			result.outputTokens = event.Usage.OutputTokens
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

		args := block.Input
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
