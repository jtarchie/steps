package agent

// assert: on an agent step — what the model said, what it decided, and which
// tools it actually called.

import (
	"fmt"
	"path/filepath"
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

// assertFilesExpectation is a step's assert.files: contract bound to the
// directory it is checked in, so anything holding one can ask whether the
// step has delivered yet without also knowing where a step's outputs live.
//
// dir is the step's SPACE, not the agent's working directory: dir: may point
// the conversation somewhere inside the space, while assert.files: paths stay
// artifact-relative to the space itself.
//
// agentDir is where the model's OWN file tools resolve a relative path
// (step.Dir). It is the same directory unless dir: moved it, and when it did,
// a nudge naming a space-relative path names one the model's tools resolve
// somewhere else — so the nudge has to say what the paths are rooted in
// rather than send it to write the same file twice in the wrong place.
type assertFilesExpectation struct {
	files    []string
	dir      string
	agentDir string
}

// newAssertFilesExpectation reads the contract off a step. The zero value is
// the right answer for a step that declares none — unmet() is empty, so every
// caller's check is a no-op without any of them testing for it.
func newAssertFilesExpectation(assert *config.Assert, dir, agentDir string) assertFilesExpectation {
	if assert == nil || len(assert.Files) == 0 {
		return assertFilesExpectation{}
	}

	return assertFilesExpectation{files: assert.Files, dir: dir, agentDir: agentDir}
}

// unmet reports the entries not satisfied right now — nothing when the step
// declares none, or when every one of them is on disk.
//
// It is the same check assertAgentResponse makes when the conversation is
// over, asked EARLY: while the model can still act on the answer. A step's
// declared files are the one part of its contract a model can be wrong about
// silently — it can believe it delivered, say so in its final message, and be
// telling the truth about its intent while the pipeline has nothing to carry
// forward. Everything downstream reads files.
func (e assertFilesExpectation) unmet() []string {
	return config.AssertFilesMismatches(e.files, e.dir)
}

// mismatch is the unmet contract as the error the post-hoc check reports, or
// nil when the step owes nothing. Shared with assertAgentResponse rather than
// reworded, so an operator reads the same sentence whichever path reached the
// failure.
func (e assertFilesExpectation) mismatch() error {
	//nolint:wrapcheck // the config package owns this message; rewording it here is the bug
	return config.AssertFilesMismatch(e.files, e.dir)
}

// nudge is what THIS step's model is told about its unmet entries: the shared
// wording, plus — when dir: moved the model's working directory out from under
// the paths — where to actually write them.
//
// Naming a space-relative path to a model whose tools resolve against dir: is
// worse than saying nothing: told "answer/reply.md does not exist" under
// `dir: answer`, a model writes <space>/answer/answer/reply.md, the assert
// stays unmet, and every remaining chance is spent on the same mistake. So
// the paths are re-expressed against the directory the model's own write_file
// resolves from, which is the one it can act on.
func (e assertFilesExpectation) nudge(unmet []string) string {
	text := assertFilesNudge(unmet)
	if e.agentDir == "" || e.agentDir == e.dir {
		return text
	}

	return text + " " + e.whereToWrite()
}

// whereToWrite translates the declared paths into the model's own frame.
//
// A path that lands outside the working directory has no such translation:
// write_file confines itself to dir (resolveWritePath), so the model
// genuinely cannot reach it and being told to try would waste its remaining
// chances. It is told where the paths are rooted instead — a shell command
// can still get there, and if nothing can, the step is misconfigured and the
// failure should read like one.
func (e assertFilesExpectation) whereToWrite() string {
	relative := make([]string, 0, len(e.files))

	for _, file := range e.files {
		rel, err := filepath.Rel(e.agentDir, filepath.Join(e.dir, file))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Sprintf("Those paths are relative to %s, which is not your working directory.", e.dir)
		}

		relative = append(relative, rel)
	}

	return fmt.Sprintf("Your working directory is not what those paths are relative to — from where you are, write them as: %s.",
		strings.Join(relative, ", "))
}

// assertFilesNudge is what a model trying to finish without its declared
// files is told.
//
// It names the paths and then names the REASON, because the reason is what
// the failure this exists for got wrong: a model that answers in its final
// message has not been lazy, it has misunderstood who is reading. Telling it
// "a later step reads these files, your message reaches nobody" corrects the
// belief; telling it "write the file" only corrects one instance of it.
//
// Deliberately not a tool result: there is no call to answer, and dressing
// one up would put words in the model's mouth about a tool it never invoked.
func assertFilesNudge(unmet []string) string {
	return fmt.Sprintf(
		"You are trying to finish, but this step declared files it must leave behind and they are not there: %s. "+
			"Your final message is not the deliverable — a later step of this pipeline reads these files, "+
			"and text you write in this conversation reaches nobody. "+
			"Write them now using the tools you have, then finish.",
		strings.Join(unmet, "; "))
}
