package agent

import (
	"errors"
	"slices"
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

	result, err := parseCLIStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	// nil expected means "no attestation to perform" — this test predates
	// attestation — but the init event is still PARSED regardless, since
	// cliRunResult.sawInit/initTools are what a caller that DOES attest reads.
	if !result.sawInit {
		t.Error("the init event was not recorded")
	}

	if want := []string{"Read", "Bash"}; !slices.Equal(result.initTools, want) {
		t.Errorf("initTools = %v, want %v", result.initTools, want)
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

	// Neither name carries the mcp__steps__ bridge prefix, so de-namespacing
	// (debridgedToolName) leaves them unchanged — these are exactly the
	// surplus-native names a real attestation failure would also flag.
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

	result, err := parseCLIStream(strings.NewReader(stream), nil, nil)
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

	result, err := parseCLIStream(strings.NewReader(stream), nil, nil)
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

	result, err := parseCLIStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if !result.isError || result.errSubtype != "error_max_turns" {
		t.Errorf("result = {isError: %v, subtype: %q}, want {true, error_max_turns}", result.isError, result.errSubtype)
	}
}

// TestParseCLIStreamRecordsTheConversation is the transcript half of the same
// parse: the same stream that yields the reduced result also has to yield the
// conversation, in the order it happened, so a CLI agent's step reads like a
// hosted one's.
func TestParseCLIStreamRecordsTheConversation(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"looking"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"main.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package main"}]}}`,
		`{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false}`,
	}, "\n")

	rec := &transcriptRecorder{}

	_, err := parseCLIStream(strings.NewReader(stream), rec, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	want := []transcriptEvent{
		{Type: "text", Text: "looking"},
		{Type: "call", Name: "Read"},
		{Type: "result", Name: "Read", Content: "package main"},
	}

	if len(rec.events) != len(want) {
		t.Fatalf("recorded %d events, want %d: %+v", len(rec.events), len(want), rec.events)
	}

	for i, expected := range want {
		got := rec.events[i]
		if got.Type != expected.Type || got.Name != expected.Name {
			t.Errorf("event %d = %s/%s, want %s/%s", i, got.Type, got.Name, expected.Type, expected.Name)
		}

		if expected.Text != "" && got.Text != expected.Text {
			t.Errorf("event %d text = %q, want %q", i, got.Text, expected.Text)
		}

		// A tool_result names only the call's id, so the content arriving
		// under the right TOOL is what proves the id lookup works.
		if expected.Content != "" && got.Content != expected.Content {
			t.Errorf("event %d content = %q, want %q", i, got.Content, expected.Content)
		}
	}
}

// TestParseCLIStreamRecordsABridgedCallOnce pins the choice not to record the
// bridge's own view as well. A bridged tool is executed by the parent AND
// reported by the child's stream; recording both would double every verdict.
// It also pins de-namespacing (issue #100): the recorded name is `verdict`,
// not `mcp__steps__verdict` — mcp__steps__ is steps' OWN bridge namespace, so
// stripping it is de-namespacing an observation, not translating one.
func TestParseCLIStreamRecordsABridgedCallOnce(t *testing.T) {
	t.Parallel()

	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"v1","name":"mcp__steps__verdict","input":{"choice":"approve"}}]}}`

	rec := &transcriptRecorder{}

	result, err := parseCLIStream(strings.NewReader(stream), rec, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if len(result.trajectory) != 1 || result.trajectory[0].name != "verdict" {
		t.Fatalf("trajectory = %+v, want one call named %q", result.trajectory, "verdict")
	}

	calls := 0

	for _, event := range rec.events {
		if event.Type == "call" {
			calls++

			if event.Name != "verdict" {
				t.Errorf("recorded transcript call name = %q, want the de-namespaced %q", event.Name, "verdict")
			}
		}
	}

	if calls != 1 {
		t.Errorf("recorded %d calls for one bridged tool_use, want 1: %+v", calls, rec.events)
	}
}

// TestParseCLIStreamKeepsAnUnrecognizedNameVerbatim: a surplus native the
// child called despite `--tools ""` has no mcp__steps__ prefix to strip, so
// it is recorded exactly as reported — a second, human-readable surplus-tool
// signal alongside the init-event attestation (cliattest.go).
func TestParseCLIStreamKeepsAnUnrecognizedNameVerbatim(t *testing.T) {
	t.Parallel()

	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"echo hi"}}]}}`

	result, err := parseCLIStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if len(result.trajectory) != 1 || result.trajectory[0].name != "Bash" {
		t.Fatalf("trajectory = %+v, want one call named %q, verbatim", result.trajectory, "Bash")
	}
}

// TestParseCLIStreamRecordsBlockShapedResults covers the other content shape:
// the Messages API sends a tool_result's content as a block array, not always
// as a bare string, and a transcript that showed raw JSON for half of them
// would be a rendering nobody could read.
func TestParseCLIStreamRecordsBlockShapedResults(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"first"},{"type":"text","text":" second"}]}]}}`,
	}, "\n")

	rec := &transcriptRecorder{}

	_, err := parseCLIStream(strings.NewReader(stream), rec, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	found := ""

	for _, event := range rec.events {
		if event.Type == "result" {
			found = event.Content
		}
	}

	if found != "first second" {
		t.Errorf("block-shaped result flattened to %q, want %q", found, "first second")
	}
}

// TestParseCLIStreamReportsWhatItSpent covers the accounting the terminal
// event carries and the parser used to discard.
func TestParseCLIStreamReportsWhatItSpent(t *testing.T) {
	t.Parallel()

	stream := `{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false,` +
		`"total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":5,` +
		`"cache_creation_input_tokens":100,"cache_read_input_tokens":900}}`

	result, err := parseCLIStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	// Cached tokens are input tokens too: a budget counts them, and the cache
	// figure says how much of that input was cheap.
	if result.inputTokens != 1010 {
		t.Errorf("input tokens = %d, want 1010", result.inputTokens)
	}

	if result.cachedTokens != 1000 {
		t.Errorf("cached tokens = %d, want 1000", result.cachedTokens)
	}

	if result.costUSD != 0.25 {
		t.Errorf("cost = %v, want 0.25", result.costUSD)
	}
}

// TestParseCLIStreamAttestationExact is the happy path: the init event
// reports exactly the expected bridged grant, and the stream is read to
// completion.
func TestParseCLIStreamAttestationExact(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["mcp__steps__read_file","mcp__steps__run_shell"]}`,
		`{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false}`,
	}, "\n")

	result, err := parseCLIStream(strings.NewReader(stream), nil, []string{"mcp__steps__read_file", "mcp__steps__run_shell"})
	if err != nil {
		t.Fatalf("parseCLIStream: %v", err)
	}

	if !result.sawResult {
		t.Error("an exact attestation match still failed to reach the result event")
	}
}

// TestParseCLIStreamAttestationSurplus is the fence actually failing: the
// child reports a native tool `--tools ""` should have withheld. The call
// must return the INSTANT the init line is parsed — proven by the later
// result event's fields never reaching cliRunResult, since parseCLIStream
// returns before the scanner ever reads that line.
func TestParseCLIStreamAttestationSurplus(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["mcp__steps__read_file","Bash"]}`,
		`{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false}`,
	}, "\n")

	result, err := parseCLIStream(strings.NewReader(stream), nil, []string{"mcp__steps__read_file"})
	if !errors.Is(err, errCLIToolSurface) {
		t.Fatalf("parseCLIStream error = %v, want errCLIToolSurface", err)
	}

	if !strings.Contains(err.Error(), "Bash") {
		t.Errorf("error %q does not name the surplus tool", err)
	}

	if result.sawResult {
		t.Error("the stream was read past the attestation failure — the later result event should never have been reached")
	}
}

// TestParseCLIStreamAttestationMissing: the init event is present but omits a
// tool the grant expected — set EQUALITY, not a subset check.
func TestParseCLIStreamAttestationMissing(t *testing.T) {
	t.Parallel()

	stream := `{"type":"system","subtype":"init","tools":["mcp__steps__read_file"]}`

	_, err := parseCLIStream(strings.NewReader(stream), nil, []string{"mcp__steps__read_file", "mcp__steps__run_shell"})
	if !errors.Is(err, errCLIToolSurface) {
		t.Fatalf("parseCLIStream error = %v, want errCLIToolSurface", err)
	}

	if !strings.Contains(err.Error(), "mcp__steps__run_shell") {
		t.Errorf("error %q does not name the missing tool", err)
	}
}

// TestParseCLIStreamAttestationAbsent covers the precedence fix: a RESULT
// with no init event is an attestation failure (the child never proved its
// tool surface), but a stream with NEITHER stays the ordinary "exited without
// reporting a result" case that execCLI's own fallback handles — parseCLIStream
// itself returns a nil error for that stream, matching its pre-attestation
// behavior.
func TestParseCLIStreamAttestationAbsent(t *testing.T) {
	t.Parallel()

	resultOnly := `{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false}`

	_, err := parseCLIStream(strings.NewReader(resultOnly), nil, []string{"mcp__steps__read_file"})
	if !errors.Is(err, errCLIToolSurface) {
		t.Fatalf("parseCLIStream error = %v, want errCLIToolSurface for a result with no init", err)
	}

	result, err := parseCLIStream(strings.NewReader(""), nil, []string{"mcp__steps__read_file"})
	if err != nil {
		t.Fatalf("an empty stream (neither event) should not be an attestation failure: %v", err)
	}

	if result.sawResult || result.sawInit {
		t.Fatalf("result = %+v, want neither flag set for an empty stream", result)
	}
}

// TestParseCLIStreamAttestationLateArriving covers a stream where the init
// event is not the first line — some CLI transcripts open with unrelated
// system events. The check still fires the instant init is actually parsed,
// wherever that is in the stream.
func TestParseCLIStreamAttestationLateArriving(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"warming up"}]}}`,
		`{"type":"system","subtype":"init","tools":["Bash"]}`,
		`{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false}`,
	}, "\n")

	_, err := parseCLIStream(strings.NewReader(stream), nil, []string{"mcp__steps__read_file"})
	if !errors.Is(err, errCLIToolSurface) {
		t.Fatalf("parseCLIStream error = %v, want errCLIToolSurface even for a late-arriving init event", err)
	}
}

// TestParseCLIStreamOtherSystemEventsAreNotAttestations is the tolerance half
// of the same rule: `system` is a family, not one event, and a CLI is free to
// grow another subtype in it. Only `init` carries a tool list, so anything
// else in the family must be skipped — reading one as an empty attestation
// would fail every step the moment the CLI shipped a new system event.
func TestParseCLIStreamOtherSystemEventsAreNotAttestations(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"type":"system","subtype":"compact_boundary"}`,
		`{"type":"system","subtype":"init","tools":["mcp__steps__read_file"]}`,
		`{"type":"result","subtype":"success","result":"done","num_turns":1,"is_error":false}`,
	}, "\n")

	result, err := parseCLIStream(strings.NewReader(stream), nil, []string{"mcp__steps__read_file"})
	if err != nil {
		t.Fatalf("an unrelated system event tripped attestation: %v", err)
	}

	if !result.sawInit || !result.sawResult {
		t.Errorf("result = %+v, want both the init and the result event seen", result)
	}
}

// TestCheckCLIToolSurface exercises the set-comparison directly, including
// that "the bridge never connected" (an empty report against a non-empty
// grant) is subsumed by the ordinary missing-tool case.
func TestCheckCLIToolSurface(t *testing.T) {
	t.Parallel()

	err := checkCLIToolSurface([]string{"b", "a"}, []string{"a", "b"})
	if err != nil {
		t.Errorf("checkCLIToolSurface: order-independent equality failed: %v", err)
	}

	err = checkCLIToolSurface(nil, []string{"mcp__steps__read_file"})
	if !errors.Is(err, errCLIToolSurface) {
		t.Errorf("checkCLIToolSurface(nil, non-empty) = %v, want errCLIToolSurface", err)
	}

	err = checkCLIToolSurface(nil, nil)
	if err != nil {
		t.Errorf("checkCLIToolSurface(nil, nil): a step granted nothing should attest cleanly: %v", err)
	}
}
