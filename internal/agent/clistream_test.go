package agent

import (
	"strings"
	"testing"
)

func TestParseCLIStream(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["Read","Bash"]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"looking"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"main.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true}]}}`,
		`{"type":"result","subtype":"success","result":"all done","num_turns":3,"is_error":false,"usage":{"input_tokens":120,"output_tokens":45}}`,
	}, "\n")

	result, err := parseCLIStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	assertCLIResultSummary(t, result)
	assertCLIResultTrajectory(t, result)
}

// assertCLIResultSummary checks the terminal result event's fields.
func assertCLIResultSummary(t *testing.T, result cliRunResult) {
	t.Helper()

	if !result.sawResult {
		t.Fatal("the terminal result event was not seen")
	}

	if result.text != "all done" || result.turns != 3 || result.isError {
		t.Errorf("result = {text: %q, turns: %d, isError: %v}, want {all done, 3, false}", result.text, result.turns, result.isError)
	}

	if result.inputTokens != 120 || result.outputTokens != 45 {
		t.Errorf("usage = %d/%d, want 120/45", result.inputTokens, result.outputTokens)
	}
}

// assertCLIResultTrajectory checks the calls and their backfilled outcomes.
func assertCLIResultTrajectory(t *testing.T, result cliRunResult) {
	t.Helper()

	if len(result.trajectory) != 2 {
		t.Fatalf("trajectory has %d calls, want 2: %+v", len(result.trajectory), result.trajectory)
	}

	// Names are recorded as the CLI reported them — what actually ran.
	if result.trajectory[0].name != "Read" || !result.trajectory[0].ok {
		t.Errorf("first call = %+v, want a successful Read", result.trajectory[0])
	}

	if result.trajectory[0].args["file_path"] != "main.go" {
		t.Errorf("first call args = %v, want file_path main.go", result.trajectory[0].args)
	}

	// The failing tool_result has to reach the trajectory, or a record of the
	// run would claim work that never landed.
	if result.trajectory[1].name != "Bash" || result.trajectory[1].ok {
		t.Errorf("second call = %+v, want a failed Bash", result.trajectory[1])
	}
}

func TestParseCLIStreamTolerance(t *testing.T) {
	t.Parallel()

	// A malformed line, an unknown event type, an unknown content block, and
	// a tool_result for a call that was never announced. The schema belongs to
	// the CLI, so none of these may fail a step that did its work.
	stream := strings.Join([]string{
		`not json at all`,
		``,
		`{"type":"some_future_event","payload":{"nested":true}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"t1","name":"Grep","input":null}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unknown"}]}}`,
		`{"type":"result","subtype":"success","result":"fine","num_turns":1}`,
	}, "\n")

	result, err := parseCLIStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if !result.sawResult || result.text != "fine" {
		t.Errorf("result = {sawResult: %v, text: %q}, want {true, fine}", result.sawResult, result.text)
	}

	if len(result.trajectory) != 1 || result.trajectory[0].name != "Grep" {
		t.Fatalf("trajectory = %+v, want one Grep call", result.trajectory)
	}

	// A tool_use with no input must still produce usable args.
	if result.trajectory[0].args == nil {
		t.Error("a call with null input produced nil args")
	}
}

func TestParseCLIStreamTruncated(t *testing.T) {
	t.Parallel()

	// The CLI died mid-conversation: calls happened, no result event. The
	// distinction from "finished with an empty answer" is what lets the driver
	// retry this and not that.
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"sleep 1"}}]}}`

	result, err := parseCLIStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if result.sawResult {
		t.Error("a truncated stream reported a result")
	}

	// The partial trajectory survives, so a timed-out step still records what
	// it managed to touch.
	if len(result.trajectory) != 1 {
		t.Errorf("trajectory = %+v, want the one call that happened before truncation", result.trajectory)
	}
}

func TestParseCLIStreamReportsFailure(t *testing.T) {
	t.Parallel()

	stream := `{"type":"result","subtype":"error_max_turns","result":"","num_turns":8,"is_error":true}`

	result, err := parseCLIStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if !result.isError || result.errSubtype != "error_max_turns" {
		t.Errorf("result = {isError: %v, subtype: %q}, want {true, error_max_turns}", result.isError, result.errSubtype)
	}
}
