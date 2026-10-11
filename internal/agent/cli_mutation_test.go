package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// Each message gets its own attempts: and must call its required tools again; only the step-wide counters carry over.
func TestCLIMessagesResetPerMessageBudgets(t *testing.T) {
	t.Parallel()

	prepared := cliPrepared(t, nil)
	prepared.conv.messages = []string{"Review the diff.", "Name the line."}

	state := newCLIStepState()

	var (
		nudges, spent, sent int
		satisfiedAtEntry    []int
		spentAtEntry        []int
	)

	attempts := func(int) error {
		spent++
		satisfiedAtEntry = append(satisfiedAtEntry, len(state.satisfied))
		spentAtEntry = append(spentAtEntry, spent)
		state.satisfied[verdictToolName] = true

		return nil
	}

	err := runCLIMessages(t.Context(), prepared, state, &nudges, &spent, &sent, attempts)
	if err != nil {
		t.Fatalf("runCLIMessages: %v", err)
	}

	if len(satisfiedAtEntry) != 2 {
		t.Fatalf("asked %d time(s), want once per message", len(satisfiedAtEntry))
	}

	if satisfiedAtEntry[1] != 0 {
		t.Error("the second message started with the first message's required tools already satisfied")
	}

	if spentAtEntry[1] != 1 {
		t.Errorf("the second message's first invocation counted as invocation %d, want its attempts: budget fresh", spentAtEntry[1])
	}
}

func TestCLIRoundAttemptsPoolsWithTheNudges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		attempts, spent, want int
	}{
		{attempts: 3, spent: 0, want: 3},
		{attempts: 3, spent: maxNudges + 1, want: 2},
		{attempts: 3, spent: maxNudges + 2, want: 1},
		{attempts: 3, spent: maxNudges + 10, want: 1},
	} {
		if got := cliRoundAttempts(tc.attempts, tc.spent); got != tc.want {
			t.Errorf("cliRoundAttempts(%d, %d) = %d, want %d", tc.attempts, tc.spent, got, tc.want)
		}
	}
}

// Once a message has reached the child, a retry of it is told to continue rather than asked the question a second time.
func TestCLIDeliveredMessageIsNotAskedAgain(t *testing.T) {
	t.Parallel()

	prepared := cliPrepared(t, nil)
	prepared.conv.messages = []string{"Review the diff.", "Name the line."}

	state := newCLIStepState()

	markDelivered(state, 1, true, nil)

	if _, pending := pendingCLIMessage(prepared, 1, state); pending {
		t.Fatal("a delivered message is still pending")
	}

	if prompt := cliAttemptPrompt(true, true, 1, state, prepared); strings.Contains(prompt, "Name the line.") {
		t.Errorf("the retry re-asked a delivered message: %q", prompt)
	}
}

func TestCLIContinuationPromptOmitsWhatItCannotSay(t *testing.T) {
	t.Parallel()

	prompt := cliContinuationPrompt(newCLIStepState(), cliPrepared(t, nil))

	if strings.Contains(prompt, "tool call(s)") {
		t.Errorf("an attempt that made no calls was told how many it made: %q", prompt)
	}

	if strings.Contains(prompt, verdictToolName) {
		t.Errorf("a step without verdicts was told to call the verdict tool: %q", prompt)
	}
}

func TestCLIReasonEdges(t *testing.T) {
	t.Parallel()

	if got := cliReason("bad\x1b[31m red\x07"); got != "bad[31m red" {
		t.Errorf("cliReason kept control runes: %q", got)
	}

	exact := strings.Repeat("a", cliReasonMaxBytes)
	if got := cliReason(exact); got != exact {
		t.Errorf("a reason exactly at the cap came back %d bytes, want it whole", len(got))
	}
}

// A stream cut short can name a call once that the bridge executed twice, and both happened.
func TestMergeCLITrajectoryKeepsARepeatedBridgedCall(t *testing.T) {
	t.Parallel()

	streamed := []recordedToolCall{{name: "read_file", ok: true}}
	bridged := []recordedToolCall{{name: "read_file", ok: true}, {name: "read_file", ok: true}}

	if merged := mergeCLITrajectory(streamed, bridged); len(merged) != 2 {
		t.Errorf("merged = %+v, want both bridged read_file calls", merged)
	}
}

func TestCleanupCLISessionRemovesItsTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	project := filepath.Join(home, ".claude", "projects", "some-project")
	err := os.MkdirAll(project, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	mine := filepath.Join(project, "11111111-2222-4333-8444-555555555555.jsonl")
	other := filepath.Join(project, "99999999-2222-4333-8444-555555555555.jsonl")

	for _, path := range []string{mine, other} {
		err = os.WriteFile(path, []byte("{}\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	cleanupCLISession("11111111-2222-4333-8444-555555555555")

	_, err = os.Stat(mine)
	if !os.IsNotExist(err) {
		t.Errorf("the step's own session transcript survived cleanup (stat: %v)", err)
	}

	_, err = os.Stat(other)
	if err != nil {
		t.Errorf("cleanup removed another session's transcript: %v", err)
	}
}

// A retry wakes a child that died and a nudge wakes one that finished; each must be told its own reason.
func TestCLIRetryAndNudgeAreToldApart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake cli binaries are shell scripts")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "asked.log")
	counter := filepath.Join(dir, "count")

	script := "#!/bin/sh\n" +
		"printf x >> " + counter + "\n" +
		"printf '%s\\n' \"--- invocation ---\" >> " + log + "\n" +
		"cat >> " + log + "\n" +
		"[ \"$(wc -c < " + counter + ")\" -eq 1 ] && exit 3\n" +
		`printf '%s\n' '` + cliInitEventJSON(nil) + "'\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","result":"answered","num_turns":1,"is_error":false}'` + "\n"

	err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700) //nolint:gosec // a test stub must be executable
	if err != nil {
		t.Fatalf("writing the fake cli: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prepared := cliPrepared(t, nil)
	prepared.ri.Attempts = 2
	prepared.conv.expect = stepExpectation{nudge: true, toolCalls: []config.ExpectedToolCall{{Name: "publish"}}}

	_, _ = runCLIConversation(t.Context(), prepared, time.Minute)

	invocations := strings.Split(askedLog(t, log), "--- invocation ---")
	if len(invocations) < 4 {
		t.Fatalf("the child was invoked %d time(s), want a failure, its retry and at least one nudge", len(invocations)-1)
	}

	retry, nudge := invocations[2], invocations[3]

	if !strings.Contains(retry, "did not finish") || !strings.Contains(retry, "publish") {
		t.Errorf("the retry of a dead attempt was told %q, want both that it died and what it owes", retry)
	}

	if strings.Contains(nudge, "did not finish") || !strings.Contains(nudge, "publish") {
		t.Errorf("the nudge of a finished attempt was told %q, want only what it owes", nudge)
	}
}
