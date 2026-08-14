package agent

// assert: on an agent step — what the model said, what it decided, and which
// tools it actually called.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

// assertAgentResponse checks an agent step's assert (stdout, verdict,
// tool_calls, and/or files — an agent has no exit code) against what the
// conversation produced. Every field that is set must pass; a mismatch on
// any is a task-level failure so the step fails and its on_failure hook
// fires. A nil assert, or one with no field set, is a no-op. dir is the
// step's own working directory (prepared.space.Dir()), checked for
// assert.files: before RunStep captures it into the artifact store.
func assertAgentResponse(assert *config.Assert, res conversationResult, dir string) error {
	if assert == nil {
		return nil
	}

	if assert.Stdout != nil && !strings.Contains(res.text, *assert.Stdout) {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(fmt.Errorf("assert.stdout: response does not contain %q", *assert.Stdout))
	}

	// An empty res.verdict here means the model finished without calling the
	// required verdict tool, which the conversation loop already treats as a
	// failure — so this reports the mismatch rather than shadowing that error.
	if assert.Verdict != nil && res.verdict != *assert.Verdict {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(fmt.Errorf("assert.verdict: want %q, got %q", *assert.Verdict, res.verdict))
	}

	if len(assert.ToolCalls) > 0 {
		err := matchToolCallTrajectory(assert.ToolCalls, res.trajectory)
		if err != nil {
			//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
			return outcome.Fail(err)
		}
	}

	err := config.AssertFilesMismatch(assert.Files, dir)
	if err != nil {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(err)
	}

	return nil
}

// matchToolCallTrajectory reports whether want appears, in order, as a
// SUBSEQUENCE of got: every expected call must be matched, in the given
// order, but any number of unexpected calls may appear before, between, or
// after them. Each expected entry matches a call with the same name whose
// arguments are a SUPERSET of the entry's args — every listed key must be
// present with an equal value, extra actual arguments ignored. Both rules are
// ported from secret-agent's eval matcher (internal/eval), which this
// feature's semantics deliberately mirror.
//
// On failure the error names the first expected call that could not be
// matched and prints the observed trajectory, so a fixture failure is
// debuggable without re-running with verbose logging.
func matchToolCallTrajectory(want []config.ExpectedToolCall, got []recordedToolCall) error {
	next := 0

	for _, expected := range want {
		matched := false

		for ; next < len(got); next++ {
			if toolCallMatches(expected, got[next]) {
				next++
				matched = true

				break
			}
		}

		if !matched {
			return fmt.Errorf("assert.tool_calls: no call matching %s after the previously matched calls; got %s",
				describeExpectedCall(expected), describeTrajectory(got))
		}
	}

	return nil
}

// toolCallMatches reports whether one observed call satisfies one expected
// entry: same name, and every expected argument present with an equal value.
// Values compare as strings via fmt.Sprint, since a tool's arguments are
// rendered into its run: template as strings regardless of the JSON type the
// model emitted.
func toolCallMatches(want config.ExpectedToolCall, got recordedToolCall) bool {
	if want.Name != got.name {
		return false
	}

	for key, wantValue := range want.Args {
		actual, present := got.args[key]
		if !present || fmt.Sprint(actual) != wantValue {
			return false
		}
	}

	return true
}

func describeExpectedCall(want config.ExpectedToolCall) string {
	if len(want.Args) == 0 {
		return fmt.Sprintf("%q", want.Name)
	}

	keys := make([]string, 0, len(want.Args))
	for key := range want.Args {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%q", key, want.Args[key]))
	}

	return fmt.Sprintf("%q with %s", want.Name, strings.Join(pairs, " "))
}

// describeTrajectory renders the observed calls for a mismatch message, names
// only — argument values can be large (a whole review body) and the name
// sequence is what makes an ordering mismatch legible.
func describeTrajectory(got []recordedToolCall) string {
	if len(got) == 0 {
		return "(no tool calls)"
	}

	names := make([]string, len(got))
	for i, call := range got {
		names[i] = call.name
	}

	return "[" + strings.Join(names, ", ") + "]"
}
