package agent

// A CLI agent asked more than one thing.
//
// The child owns its own turn loop and exits when it is done, so the only way
// to ask it a second question is to resume the session it just left — the same
// mechanism a died attempt and a missing-file nudge already use.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// recordingCLI puts a fake CLI on PATH that appends each invocation's stdin to
// a log and answers with a valid result event, and returns the log's path.
// grantedTools names the tools the step granted (steps' own bare names,
// e.g. "read_file") — reported back on the init event exactly as the bridge
// would name them, since the attestation check (cliattest.go) fails a step
// whose fake CLI does not honestly report its tool surface.
//
// One line per invocation, so a test can assert both WHAT the child was asked
// and how many times it was woken.
func recordingCLI(t *testing.T, grantedTools ...string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake cli binaries are shell scripts")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "asked.log")

	initEvent := cliInitEventJSON(grantedTools)

	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"--- invocation ---\" >> " + log + "\n" +
		"cat >> " + log + "\n" +
		"printf '\\n' >> " + log + "\n" +
		`printf '%s\n' '` + initEvent + "'\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","result":"answered","num_turns":1,"is_error":false}'` + "\n"

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing the fake cli: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

// cliInitEventJSON renders a system/init event reporting exactly the bridged
// spelling of grantedTools — the shape a well-behaved child's init event
// would carry for that grant.
func cliInitEventJSON(grantedTools []string) string {
	bridged := make([]string, len(grantedTools))
	for i, name := range grantedTools {
		bridged[i] = `"` + bridgedToolName(name) + `"`
	}

	return `{"type":"system","subtype":"init","tools":[` + strings.Join(bridged, ",") + `]}`
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
	log := recordingCLI(t, "read_file")

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

// spendingCLI is recordingCLI's twin for the pooled ceilings: the child
// reports `turns` turns and `cost` dollars on every invocation, so one
// invocation can be made to spend a whole step's allowance.
func spendingCLI(t *testing.T, turns int, cost float64) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake cli binaries are shell scripts")
	}

	dir := t.TempDir()

	result := fmt.Sprintf(
		`{"type":"result","subtype":"success","result":"answered","num_turns":%d,"total_cost_usd":%v,"is_error":false}`,
		turns, cost)

	// Every caller of spendingCLI grants no tools (cliPrepared(t, nil)), so an
	// empty init event is what a well-behaved child would report.
	script := "#!/bin/sh\ncat > /dev/null\n" +
		`printf '%s\n' '` + cliInitEventJSON(nil) + "'\n" +
		`printf '%s\n' '` + result + "'\n"

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing the fake cli: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCLICeilingNamesTheMessageItStoppedOn is the seam the pooled ceilings
// actually break on, and the reason this is an integration test rather than a
// unit test of the formatter: message one finishes CLEANLY and spends the
// whole allowance, and message two is refused before it is ever asked.
//
// Both halves of that sentence were unreportable. `lastErr` is nil precisely
// because the previous message succeeded — the common case, since max_turns:
// does not reset at a message boundary — so wrapping it put the literal text
// "%!w(<nil>)" at the end of the one line explaining the failure. And the
// attempt counter belongs to the message being asked NOW, so a step that had
// spent a real conversation's worth of turns reported "0 attempt(s)" and read
// as a step that never started.
func TestCLICeilingNamesTheMessageItStoppedOn(t *testing.T) {
	spendingCLI(t, 30, 0)

	prepared := cliPrepared(t, nil)
	prepared.ri.MaxTurns = 30
	prepared.conv.messages = []string{"Plan it.", "Now do it.", "Write it up."}

	_, err := runCLIConversation(t.Context(), prepared, time.Minute)
	if err == nil {
		t.Fatal("a step whose first message spent the whole turn budget succeeded")
	}

	got := err.Error()

	if strings.Contains(got, "%!") {
		t.Errorf("the failure carries a formatting error where a cause should be: %s", got)
	}

	if !strings.Contains(got, "message 2 of 3") {
		t.Errorf("the failure does not say which message was never asked: %s", got)
	}

	// The attempt clause belongs to message two, which was never tried: said
	// in words, because "0 attempt(s)" is the phrase that read as a step
	// that never started.
	if !strings.Contains(got, "before its first attempt at it") || strings.Contains(got, "0 attempt") {
		t.Errorf("the failure still counts attempts at a message that was never asked: %s", got)
	}
}

// TestCLIBudgetCeilingNamesTheMessageToo keeps the other pooled ceiling
// honest. It is the same shape and the same nil cause, and fixing only the
// turn branch would leave the dollar branch printing "%!w(<nil>)" for exactly
// the run that spent the most money.
func TestCLIBudgetCeilingNamesTheMessageToo(t *testing.T) {
	spendingCLI(t, 1, 5)

	prepared := cliPrepared(t, nil)
	prepared.ri.BudgetUSD = 5
	prepared.conv.messages = []string{"Plan it.", "Now do it."}

	_, err := runCLIConversation(t.Context(), prepared, time.Minute)
	if err == nil {
		t.Fatal("a step whose first message spent the whole dollar budget succeeded")
	}

	got := err.Error()

	if strings.Contains(got, "%!") {
		t.Errorf("the failure carries a formatting error where a cause should be: %s", got)
	}

	if !strings.Contains(got, "message 2 of 2") {
		t.Errorf("the failure does not say which message was never asked: %s", got)
	}
}

// TestCLICeilingKeepsTheFailureThatCausedIt is the other direction, and the
// reason the cause is dropped CONDITIONALLY rather than removed: when the
// allowance ran out because attempts kept dying, that error is the thing worth
// investigating and the ceiling is only how the step finally stopped.
func TestCLICeilingKeepsTheFailureThatCausedIt(t *testing.T) {
	t.Parallel()

	cause := errors.New("the daemon went away")
	got := cliCeilingError("reviewer", "its 9-turn budget", 1, 2, 2, cause)

	if !strings.Contains(got.Error(), "the daemon went away") {
		t.Errorf("the ceiling swallowed the failure that caused it: %s", got)
	}

	// Wrapped, not merely printed: a consumer reaching past the ceiling for
	// the real outage does it with errors.Is/As.
	if !errors.Is(got, cause) {
		t.Error("the cause is not unwrappable; errors.Is past the ceiling stops working")
	}

	// And the nil case says nothing about a cause at all, rather than saying
	// it in a way fmt cannot render.
	quiet := cliCeilingError("reviewer", "its 9-turn budget", 1, 2, 2, nil)
	if strings.Contains(quiet.Error(), "last failure") || strings.Contains(quiet.Error(), "%!") {
		t.Errorf("a ceiling with no cause still claims one: %s", quiet)
	}
}

// TestCeilingErrorNamesOneMessageForAPromptOnlyStep: runCLIMessages
// substitutes a single empty entry when a step declares no messages:, so the
// count the error quotes has to agree with it. Reading the raw slice length
// made the overwhelmingly common case — one prompt: — report "on message 1 of
// 0", which reads as a step that never started.
func TestCeilingErrorNamesOneMessageForAPromptOnlyStep(t *testing.T) {
	t.Parallel()

	err := cliCeilingError("impl", "its $0.5 budget, spending $0.5100", 0, 0, 1, nil)

	if !strings.Contains(err.Error(), "on message 1 of 1") {
		t.Errorf("a prompt-only step reports %q", err)
	}
}

// TestCeilingErrorWithoutAFailureDoesNotWrapNil: the ordinary way to exhaust a
// pooled ceiling is for an EARLIER message to finish cleanly having spent it,
// which leaves no last failure — and %w on a nil error printed
// "%!w(<nil>)" as the last word of the one line explaining the step.
func TestCeilingErrorWithoutAFailureDoesNotWrapNil(t *testing.T) {
	t.Parallel()

	err := cliCeilingError("impl", "its 30-turn budget", 2, 4, 1, nil)

	if strings.Contains(err.Error(), "%!w") || strings.Contains(err.Error(), "last failure") {
		t.Errorf("a ceiling reached with nothing failing reports %q", err)
	}

	if !strings.Contains(err.Error(), "on message 3 of 4") {
		t.Errorf("the message counter is off: %q", err)
	}
}
