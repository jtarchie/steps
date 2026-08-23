package agent

// A CLI agent asked more than one thing.
//
// The child owns its own turn loop and exits when it is done, so the only way
// to ask it a second question is to resume the session it just left — the same
// mechanism a died attempt and a missing-file nudge already use.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordingCLI puts a fake CLI on PATH that appends each invocation's stdin to
// a log and answers with a valid result event, and returns the log's path.
//
// One line per invocation, so a test can assert both WHAT the child was asked
// and how many times it was woken.
func recordingCLI(t *testing.T) string {
	t.Helper()

	if os.Getenv("STEPS_TEST_SKIP_SHELL") != "" {
		t.Skip("fake cli binaries are shell scripts")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "asked.log")

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"--- invocation ---\" >> " + log + "\n" +
		"cat >> " + log + "\n" +
		"printf '\\n' >> " + log + "\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","result":"answered","num_turns":1,"is_error":false}'` + "\n"

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing the fake cli: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

func askedLog(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a path this test just built
	if err != nil {
		t.Fatalf("reading what the cli was asked: %v", err)
	}

	return string(data)
}

// TestCLIAgentIsAskedEveryMessage is the feature: a CLI agent gets every
// message, each one resuming the session the last was answered in.
//
// Before this, the second message was assembled into nothing and the step went
// green having asked half the question — the failure mode that made a load
// error the alternative.
func TestCLIAgentIsAskedEveryMessage(t *testing.T) {
	log := recordingCLI(t)

	prepared := cliPrepared(t, nil)
	prepared.conv.messages = []string{"Review the diff.", "Name the line it turns on."}

	_, err := runCLIConversation(t.Context(), prepared, time.Minute)
	if err != nil {
		t.Fatalf("runCLIConversation: %v", err)
	}

	asked := askedLog(t, log)

	if got := strings.Count(asked, "--- invocation ---"); got != 2 {
		t.Fatalf("the child was invoked %d time(s), want one per message:\n%s", got, asked)
	}

	if !strings.Contains(asked, "Review the diff.") {
		t.Error("the first message never reached the child")
	}

	if !strings.Contains(asked, "Name the line it turns on.") {
		t.Error("the second message never reached the child — it was silently dropped")
	}
}

// TestCLIAgentSecondMessageResumesRatherThanRestarts pins what makes this one
// conversation. A resumed invocation is sent the message and nothing else: the
// session already holds the task and its context blocks, and re-sending them
// is what invites the child to redo work it has already finished.
func TestCLIAgentSecondMessageResumesRatherThanRestarts(t *testing.T) {
	log := recordingCLI(t)

	prepared := cliPrepared(t, []string{"read_file"})
	prepared.conv.messages = []string{"Review the diff.", "Name the line it turns on."}
	prepared.conv.contextBlocks = []contextBlock{{path: "repo/NOTES.md", content: "some notes"}}

	_, err := runCLIConversation(t.Context(), prepared, time.Minute)
	if err != nil {
		t.Fatalf("runCLIConversation: %v", err)
	}

	asked := askedLog(t, log)

	invocations := strings.Split(asked, "--- invocation ---")
	if len(invocations) != 3 { // the split leaves an empty leading element
		t.Fatalf("expected two invocations, got %d:\n%s", len(invocations)-1, asked)
	}

	first, second := invocations[1], invocations[2]

	if !strings.Contains(first, "some notes") {
		t.Error("the opening invocation did not carry the step's context blocks")
	}

	if strings.Contains(second, "some notes") {
		t.Error("the resumed invocation re-sent the context blocks — it is restarting the task, not continuing it")
	}

	if strings.Contains(second, "Review the diff.") {
		t.Error("the resumed invocation re-sent the first message")
	}
}

// TestCLIAgentOneMessageIsOneInvocation pins that the common case did not move.
func TestCLIAgentOneMessageIsOneInvocation(t *testing.T) {
	log := recordingCLI(t)

	prepared := cliPrepared(t, nil)
	prepared.conv.messages = []string{"Review the diff."}

	_, err := runCLIConversation(t.Context(), prepared, time.Minute)
	if err != nil {
		t.Fatalf("runCLIConversation: %v", err)
	}

	if got := strings.Count(askedLog(t, log), "--- invocation ---"); got != 1 {
		t.Fatalf("the child was invoked %d time(s), want exactly 1", got)
	}
}

// TestCLIAttemptPromptDoesNotConsumeTheMessage pins that composing a prompt
// has no side effect.
//
// It used to mark the message as asked while merely building the string, so
// any failure before the child received it — the bridge config, a pipe, the
// spawn — made the retry send "continue" for a question that was never put.
// The step then reported success having skipped it, silently.
func TestCLIAttemptPromptDoesNotConsumeTheMessage(t *testing.T) {
	t.Parallel()

	prepared := cliPrepared(t, nil)
	prepared.conv.messages = []string{"Review the diff.", "Name the line."}

	state := newCLIStepState()

	first := cliAttemptPrompt(true, false, 1, state, prepared)
	if !strings.Contains(first, "Name the line.") {
		t.Fatalf("prompt = %q, want the pending message", first)
	}

	// The same invocation, retried because it never reached the child.
	second := cliAttemptPrompt(true, true, 1, state, prepared)
	if !strings.Contains(second, "Name the line.") {
		t.Errorf("the retry asked %q — the message was consumed by composing the first prompt, so the child is told to continue something it never got", second)
	}
}
