package agent

// The branches of the assert.files: nudge the end-to-end pass cannot reach:
// the allowance running out, the verdict gate, and the exact wording the
// model is handed.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// expectingConversation builds a conversation whose step declares files it
// must leave behind, over the default grant plus write_file.
func expectingConversation(t *testing.T, dir string, files ...string) agentConversation {
	t.Helper()

	specs := append(config.DefaultAgentToolSpecs(), config.ToolSpec{Builtin: "write_file"})

	built, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		prompt:   "answer it",
		env:      toolEnv{dir: dir, runner: runner},
		tools:    built,
		maxTurns: testMaxTurns,
		expect:   newAssertFilesExpectation(&config.Assert{Files: files}, dir),
	}
}

// TestNudgeStopsAfterItsAllowance is the runaway guard: a model that will
// never write the file must be let go, not asked forever. It also pins that
// the loop does NOT fail here — the conversation ends clean and
// assertAgentResponse is left to report the mismatch, so there is exactly one
// place that decides what an unmet assert means.
func TestNudgeStopsAfterItsAllowance(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: refusals("The answer is in this message.")}

	res, err := runAgentConversation(context.Background(), fake, expectingConversation(t, dir, "answer/reply.md"))
	if err != nil {
		t.Fatalf("the conversation itself should end clean; the assert is what fails: %v", err)
	}

	if res.text != "The answer is in this message." {
		t.Errorf("final text = %q, want the model's own last answer", res.text)
	}

	// One stop attempt, then maxFilesNudges more — and no more than that,
	// which is what proves the counter never resets.
	if want := maxFilesNudges + 1; fake.calls != want {
		t.Errorf("provider saw %d turns, want %d (the stop attempt plus %d nudges)", fake.calls, want, maxFilesNudges)
	}
}

// TestNudgeNamesEveryMissingFileAtOnce pins the plural: told one file at a
// time, a model pays a turn per artifact to learn something one sentence
// could have said.
func TestNudgeNamesEveryMissingFileAtOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: refusals("Done!")}

	_, err := runAgentConversation(context.Background(), fake,
		expectingConversation(t, dir, "out/a.md", "out/b.md"))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	nudge := lastUserText(t, fake.requests[1])
	for _, want := range []string{"out/a.md", "out/b.md"} {
		if !strings.Contains(nudge, want) {
			t.Errorf("nudge does not name %s: %q", want, nudge)
		}
	}
}

// TestNudgeStopsOnceTheFileArrives proves the counter tracks the CONTRACT and
// not the model's manners: a model that writes what it owes is never nudged
// again, and never nudged at all if it wrote it before trying to stop.
func TestNudgeStopsOnceTheFileArrives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeExpectedFile(t, dir, "answer/reply.md")

	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Written and done.")}}

	_, err := runAgentConversation(context.Background(), fake, expectingConversation(t, dir, "answer/reply.md"))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if fake.calls != 1 {
		t.Errorf("provider saw %d turns, want 1 — a satisfied contract is not worth a word", fake.calls)
	}
}

// TestNudgeTreatsAnEmptyFileAsMissing covers the shape a model reaches for
// when it is complying with the letter of a nudge: touch the path, finish.
func TestNudgeTreatsAnEmptyFileAsMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "answer"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "answer", "reply.md"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeLLM{responses: refusals("Done.")}

	_, err = runAgentConversation(context.Background(), fake, expectingConversation(t, dir, "answer/reply.md"))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if fake.calls < 2 {
		t.Fatal("an empty declared file was accepted as delivered")
	}

	if nudge := lastUserText(t, fake.requests[1]); !strings.Contains(nudge, "is empty") {
		t.Errorf("nudge does not say the file is empty: %q", nudge)
	}
}

// TestVerdictRefusedWhileFilesAreMissing is the gate. A verdict is the model's
// report on the work and the files ARE the work, so a decision recorded over
// missing artifacts is the exact false success this whole contract exists to
// prevent: the model reports, routing fires on the report, the run is green,
// and nothing was produced.
func TestVerdictRefusedWhileFilesAreMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: refusals("")}
	fake.responses[0] = verdictCall("approve")

	res, _ := runAgentConversation(context.Background(), fake,
		expectingVerdictConversation(t, dir, []string{"approve", "revise"}, "answer/reply.md"))

	if res.verdict != "" {
		t.Errorf("verdict = %q, want none recorded: the step's declared file was never written", res.verdict)
	}
}

// TestVerdictAcceptedOnceFilesExist is the control for the gate above — the
// refusal has to be about the missing file, not about verdicts generally.
func TestVerdictAcceptedOnceFilesExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeExpectedFile(t, dir, "answer/reply.md")

	fake := &fakeLLM{responses: []*model.LLMResponse{verdictCall("approve"), finalText("done")}}

	res, err := runAgentConversation(context.Background(), fake,
		expectingVerdictConversation(t, dir, []string{"approve", "revise"}, "answer/reply.md"))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.verdict != "approve" {
		t.Errorf("verdict = %q, want approve", res.verdict)
	}
}

// expectingVerdictConversation is verdictConversation with a file contract,
// bound into the synthesized tool the way prepareAgentStep binds it: the gate
// lives in the tool's closure, so setting it on the conversation afterwards
// would test nothing.
func expectingVerdictConversation(t *testing.T, dir string, verdicts []string, files ...string) agentConversation {
	t.Helper()

	expect := newAssertFilesExpectation(&config.Assert{Files: files}, dir)

	built, _, err := buildAgentTools(context.Background(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	verdictTool, err := injectVerdictTool(verdicts, false, built, expect)
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		prompt:      "judge it",
		env:         toolEnv{dir: dir, runner: runner},
		tools:       built,
		maxTurns:    testMaxTurns,
		verdictTool: verdictTool,
		expect:      expect,
	}
}

// refusals scripts a model that answers with the same text for longer than
// the nudge allowance, so a test asserting on the nudge never runs out of
// provider before the loop runs out of patience.
func refusals(text string) []*model.LLMResponse {
	out := make([]*model.LLMResponse, maxFilesNudges+2)
	for i := range out {
		out[i] = textResponse(text)
	}

	return out
}

// writeExpectedFile creates a non-empty file at an artifact-relative path.
func writeExpectedFile(t *testing.T, dir, rel string) {
	t.Helper()

	path := filepath.Join(dir, rel)

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte("delivered"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// lastUserText returns the final plain-text user message of a request — what
// the model was told immediately before the turn it is about to take.
func lastUserText(t *testing.T, req *model.LLMRequest) string {
	t.Helper()

	for i := len(req.Contents) - 1; i >= 0; i-- {
		content := req.Contents[i]
		if content.Role != genai.RoleUser {
			continue
		}

		for _, part := range content.Parts {
			if part.Text != "" {
				return part.Text
			}
		}
	}

	t.Fatal("no user text in the request")

	return ""
}
