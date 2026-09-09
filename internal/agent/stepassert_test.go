package agent

// The assert.tool_calls: matcher and the nudge built on it. These semantics
// were comment-and-code with nothing holding them until this file, which is
// how the ordering rule came to be misremembered as unordered.

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func calls(names ...string) []recordedToolCall {
	out := make([]recordedToolCall, len(names))
	for i, name := range names {
		out[i] = recordedToolCall{name: name}
	}

	return out
}

func wants(names ...string) []config.ExpectedToolCall {
	out := make([]config.ExpectedToolCall, len(names))
	for i, name := range names {
		out[i] = config.ExpectedToolCall{Name: name}
	}

	return out
}

// TestMatchToolCallTrajectoryIsAnOrderedSubsequence pins the rule: every
// expected call, in order, with any number of extras before, between or
// after — and NOT the same calls in another order.
func TestMatchToolCallTrajectoryIsAnOrderedSubsequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want []config.ExpectedToolCall
		got  []recordedToolCall
		ok   bool
	}{
		{name: "exact", want: wants("a", "b"), got: calls("a", "b"), ok: true},
		{name: "gap between matches", want: wants("a", "b"), got: calls("a", "c", "b"), ok: true},
		{name: "leading and trailing extras", want: wants("a", "b"), got: calls("c", "a", "b", "c"), ok: true},
		{name: "order violated", want: wants("a", "b"), got: calls("b", "a"), ok: false},
		{name: "second missing", want: wants("a", "b"), got: calls("a"), ok: false},
		{name: "nothing called", want: wants("a"), got: nil, ok: false},
		{name: "repeated want needs repeated call", want: wants("a", "a"), got: calls("a"), ok: false},
		{name: "recovered by calling again in order", want: wants("a", "b"), got: calls("b", "a", "b"), ok: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := matchToolCallTrajectory(tt.want, tt.got)
			if (err == nil) != tt.ok {
				t.Fatalf("match = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// TestMatchToolCallTrajectoryArgsAreASubset pins that an expected entry's
// args must all be present and equal, extras on the actual call ignored, and
// values compared as strings.
func TestMatchToolCallTrajectoryArgsAreASubset(t *testing.T) {
	t.Parallel()

	want := []config.ExpectedToolCall{{Name: "write", Args: map[string]string{"path": "out.md", "n": "3"}}}

	superset := []recordedToolCall{{name: "write", args: map[string]any{"path": "out.md", "n": 3, "extra": true}}}

	err := matchToolCallTrajectory(want, superset)
	if err != nil {
		t.Errorf("a superset with equal values should match: %v", err)
	}

	differs := []recordedToolCall{{name: "write", args: map[string]any{"path": "other.md", "n": 3}}}

	err = matchToolCallTrajectory(want, differs)
	if err == nil {
		t.Error("a differing value matched")
	}

	missing := []recordedToolCall{{name: "write", args: map[string]any{"path": "out.md"}}}

	err = matchToolCallTrajectory(want, missing)
	if err == nil {
		t.Error("a missing key matched")
	}
}

// TestMatchToolCallTrajectoryNamesTheFirstUnmatched pins that the failure
// names the entry the cursor stopped on and the whole observed sequence.
func TestMatchToolCallTrajectoryNamesTheFirstUnmatched(t *testing.T) {
	t.Parallel()

	err := matchToolCallTrajectory(wants("a", "b", "c"), calls("a", "x"))
	if err == nil {
		t.Fatal("expected a mismatch")
	}

	for _, want := range []string{`"b"`, "[a, x]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %s", err, want)
		}
	}
}

// TestOwedIsSilentWithoutOptIn pins that a step which did not set nudge:
// owes nothing early, however unmet its contract is — the zero value and an
// assert without the flag alike.
func TestOwedIsSilentWithoutOptIn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	assert := &config.Assert{Files: []string{"answer/reply.md"}, ToolCalls: wants("run_tests")}

	for name, expect := range map[string]stepExpectation{
		"zero value": {},
		"flag unset": newStepExpectation(assert, dir, dir),
		"nil assert": newStepExpectation(nil, dir, dir),
	} {
		unmet, message := expect.owed(nil)
		if len(unmet) != 0 || message != "" {
			t.Errorf("%s: owed = %v, %q; want nothing", name, unmet, message)
		}

		err := expect.mismatch(nil)
		if err != nil {
			t.Errorf("%s: mismatch = %v; want nil", name, err)
		}
	}
}

// TestOwedNamesFilesAndCallsInOneMessage pins the one-message rule and the
// wording split: the files sentence corrects a belief, the tool_calls
// sentence is mechanical and names only what remains, in order.
func TestOwedNamesFilesAndCallsInOneMessage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	expect := newStepExpectation(&config.Assert{
		Files:     []string{"answer/reply.md"},
		ToolCalls: wants("run_tests", "post_review"),
		Nudge:     true,
	}, dir, dir)

	unmet, message := expect.owed(calls("read_file"))

	if len(unmet) != 3 {
		t.Fatalf("unmet = %v, want the file and both calls", unmet)
	}

	for _, want := range []string{
		"answer/reply.md does not exist",
		"final message is not the deliverable",
		"also declared tool calls",
		"run_tests, then post_review",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not contain %q", message, want)
		}
	}

	if strings.Count(message, "then finish") != 2 {
		t.Errorf("message %q should carry both halves once each", message)
	}
}

// TestOwedReportsWhereTheCursorStopped pins that a partly-followed procedure
// is told only its remainder, and that calls out of order are recoverable by
// calling again — no omission-versus-order split.
func TestOwedReportsWhereTheCursorStopped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	expect := newStepExpectation(&config.Assert{ToolCalls: wants("run_tests", "post_review"), Nudge: true}, dir, dir)

	unmet, message := expect.owed(calls("run_tests"))
	if len(unmet) != 1 || unmet[0] != "post_review" {
		t.Errorf("after run_tests, unmet = %v; want [post_review]", unmet)
	}

	if strings.Contains(message, "also declared") {
		t.Errorf("no files owed, but the message read as a second half: %q", message)
	}

	if !strings.Contains(message, "in this order: post_review.") {
		t.Errorf("message %q does not name only the remainder", message)
	}

	if unmet, _ := expect.owed(calls("post_review")); len(unmet) != 2 {
		t.Errorf("post_review before run_tests should still owe both, in order; got %v", unmet)
	}

	if unmet, _ := expect.owed(calls("post_review", "run_tests", "post_review")); len(unmet) != 0 {
		t.Errorf("calling again in order should satisfy; got %v", unmet)
	}
}

// TestOwedFilesOnlyIsTheFilesNudge pins that a files-only step gets exactly
// the wording it always did, with no tool_calls sentence appended.
func TestOwedFilesOnlyIsTheFilesNudge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	expect := newStepExpectation(&config.Assert{Files: []string{"answer/reply.md"}, Nudge: true}, dir, dir)

	_, message := expect.owed(nil)
	if message != assertFilesNudge([]string{"answer/reply.md does not exist"}) {
		t.Errorf("message = %q, want the bare files nudge", message)
	}

	writeExpectedFile(t, dir, "answer/reply.md")

	if unmet, message := expect.owed(nil); len(unmet) != 0 || message != "" {
		t.Errorf("file written, still owed: %v %q", unmet, message)
	}
}

// TestMismatchPrefersTheFile pins what blameUnmet reports when both are owed:
// the deliverable, in the post-hoc wording, over the procedure.
func TestMismatchPrefersTheFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	expect := newStepExpectation(&config.Assert{
		Files:     []string{"answer/notes.md"},
		ToolCalls: wants("run_tests"),
		Nudge:     true,
	}, dir, dir)

	err := expect.mismatch(nil)
	if err == nil || !strings.HasPrefix(err.Error(), "assert.files:") {
		t.Errorf("mismatch = %v, want the files error first", err)
	}

	writeExpectedFile(t, dir, "answer/notes.md")

	err = expect.mismatch(nil)
	if err == nil || !strings.HasPrefix(err.Error(), "assert.tool_calls:") {
		t.Errorf("mismatch = %v, want the tool_calls error once the file exists", err)
	}

	err = expect.mismatch(calls("run_tests"))
	if err != nil {
		t.Errorf("mismatch = %v, want nil once both are met", err)
	}
}

// TestVerdictTrajectoryCountsTheCallBeingMade pins the seam between the two
// record-keepers: the hosted loop has already appended the verdict call when
// the gate runs, the bridge has not, and the gate must see the same thing.
func TestVerdictTrajectoryCountsTheCallBeingMade(t *testing.T) {
	t.Parallel()

	args := map[string]any{"choice": "approve"}

	hosted := toolEnv{trajectory: func() []recordedToolCall { return calls("run_tests", verdictToolName) }}
	if got := verdictTrajectory(hosted, args); len(got) != 2 {
		t.Errorf("hosted: verdict appended twice: %v", got)
	}

	bridged := toolEnv{trajectory: func() []recordedToolCall { return calls("run_tests") }}
	got := verdictTrajectory(bridged, args)
	if len(got) != 2 || got[1].name != verdictToolName {
		t.Errorf("bridged: verdict call not counted: %v", got)
	}

	if got := verdictTrajectory(toolEnv{}, args); len(got) != 1 || got[0].name != verdictToolName {
		t.Errorf("outside a conversation: %v", got)
	}
}

// TestAssertAgentResponseHintsAtNudgeOnlyWhenOff pins the migration clause:
// a step that never opted in is told the flag exists; one that did and still
// failed is not told to set what it already set.
func TestAssertAgentResponseHintsAtNudgeOnlyWhenOff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	res := conversationResult{text: "done", trajectory: nil}

	off := assertAgentResponse(&config.Assert{Files: []string{"answer/reply.md"}}, res, dir)
	if off == nil || !strings.Contains(off.Error(), "assert.nudge: true") {
		t.Errorf("without the flag: %v, want the hint", off)
	}

	on := assertAgentResponse(&config.Assert{Files: []string{"answer/reply.md"}, Nudge: true}, res, dir)
	if on == nil || strings.Contains(on.Error(), "assert.nudge: true") {
		t.Errorf("with the flag: %v, want no hint", on)
	}

	procedure := assertAgentResponse(&config.Assert{ToolCalls: wants("run_tests")}, res, dir)
	if procedure == nil || !strings.Contains(procedure.Error(), "assert.nudge: true") {
		t.Errorf("tool_calls without the flag: %v, want the hint", procedure)
	}
}
