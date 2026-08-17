package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestAgentTimeoutResolution pins the one field whose EMPTY value is not
// "no limit". An agent step is the only kind that gets a deadline it never
// asked for, so it is also the only kind that needs a way to decline one.
func TestAgentTimeoutResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"unset takes the package default", "", agentStepTimeout},
		{"a duration is honored", "45m", 45 * time.Minute},
		{"0 is no deadline", "0", noAgentDeadline},
		{"0s likewise", "0s", noAgentDeadline},
		// Not noAgentDeadline: a value nobody can parse is a typo, and
		// resolving a typo into an unbounded step is the wrong direction to
		// fail in. LoadConfig rejects one long before this anyway.
		{"an unparseable value falls back to the default", "twenty minutes", agentStepTimeout},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := agentTimeout(test.in); got != test.want {
				t.Errorf("agentTimeout(%q) = %v, want %v", test.in, got, test.want)
			}
		})
	}
}

// TestWithAgentDeadlineLeavesAnUncappedContextAlone is the assertion that
// keeps timeout: 0 from meaning its opposite: context.WithTimeout(ctx, 0)
// hands back an ALREADY-expired context, so passing the resolved zero through
// would fail every uncapped step instantly.
func TestWithAgentDeadlineLeavesAnUncappedContextAlone(t *testing.T) {
	t.Parallel()

	ctx, cancel := withAgentDeadline(context.Background(), noAgentDeadline)
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("an uncapped step's context carries a deadline")
	}

	err := ctx.Err()
	if err != nil {
		t.Errorf("ctx.Err() = %v, want nil — the context expired before the step ran", err)
	}

	bounded, cancelBounded := withAgentDeadline(context.Background(), time.Hour)
	defer cancelBounded()

	if _, ok := bounded.Deadline(); !ok {
		t.Error("a bounded step's context carries no deadline")
	}
}

// TestRemainingCLITurnsCarriesTheSentinel covers the arithmetic that would
// otherwise give an uncapped step a cap: the CLI path spends turns across
// attempts and subtracts them from the budget, and "no cap" minus anything
// is still no cap.
func TestRemainingCLITurnsCarriesTheSentinel(t *testing.T) {
	t.Parallel()

	if got := remainingCLITurns(0, 17); got != unlimitedTurns {
		t.Errorf("remainingCLITurns(0, 17) = %d, want the unlimited sentinel", got)
	}

	if got := remainingCLITurns(30, 12); got != 18 {
		t.Errorf("remainingCLITurns(30, 12) = %d, want 18", got)
	}

	// The CLI reports num_turns in its own units, so a capped step can come
	// back having spent more than its ceiling. A bare subtraction lands on
	// exactly the sentinel here, which would read as "uncapped" and hand the
	// next attempt no --max-turns at all.
	overspent := remainingCLITurns(30, 31)
	if overspent == unlimitedTurns {
		t.Fatal("an overspent budget collided with the unlimited sentinel")
	}

	if !(cliAttempt{maxTurns: overspent}).outOfTurns() {
		t.Errorf("remainingCLITurns(30, 31) = %d, which does not read as an exhausted budget", overspent)
	}
}

// TestIgnoredForcesEndTheConversation covers the path the loop detector
// structurally cannot see: a provider that disregards tool_choice, so the
// model answers with text while a required tool goes unmade. The detector
// hashes tool INTERACTIONS, and these turns produce none.
//
// max_turns used to be the bound here — the conversation loop's own comment
// said so — which stopped being true the moment max_turns: 0 became
// expressible. Uncapped, this is a loop with nothing to end it.
func TestIgnoredForcesEndTheConversation(t *testing.T) {
	t.Parallel()

	const toolName = "submit"

	// Answers with text every time, never calling the required tool: the
	// provider-ignores-tool_choice case. More responses than the bound so a
	// regression loops rather than running out of script.
	responses := make([]*model.LLMResponse, 0, 100)
	for range 100 {
		responses = append(responses, &model.LLMResponse{Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: "I would rather just tell you."}},
		}})
	}

	fake := &fakeLLM{responses: responses}

	conv := agentConversation{
		prompt: "submit your answer",
		env:    toolEnv{dir: t.TempDir()},
		tools: agentTools{
			registry: map[string]toolImpl{toolName: func(context.Context, map[string]any, toolEnv) map[string]any { return nil }},
			required: map[string]bool{toolName: true},
		},
		maxTurns: 0, // the uncapped case: nothing else can stop this
	}

	_, err := runAgentConversation(t.Context(), fake, conv)
	if err == nil {
		t.Fatal("an uncapped conversation that never made its required call returned no error")
	}

	if !strings.Contains(err.Error(), toolName) {
		t.Errorf("error = %v, want one naming the required tool that never succeeded", err)
	}

	if fake.calls > maxIgnoredForces+1 {
		t.Errorf("provider requests = %d, want it bounded near %d", fake.calls, maxIgnoredForces)
	}
}

// TestCLIArgsOmitsMaxTurnsWhenUncapped pins the argument vector, which is
// where a turn cap becomes real for a CLI-backed agent. The CLIs steps drives
// impose no cap of their own, so passing no flag IS the uncapped spelling —
// any number would be a ceiling the pipeline never asked for.
func TestCLIArgsOmitsMaxTurnsWhenUncapped(t *testing.T) {
	t.Parallel()

	plan := firstAttempt()
	plan.maxTurns = unlimitedTurns

	args := cliArgs(cliPrepared(t, nil), cliRuntimes["claude"], "/tmp/mcp.json", plan)
	if slices.Contains(args, "--max-turns") {
		t.Errorf("args = %v, want no --max-turns for an uncapped step", args)
	}

	capped := cliArgs(cliPrepared(t, nil), cliRuntimes["claude"], "/tmp/mcp.json", firstAttempt())
	if got := argValue(capped, "--max-turns"); got != "12" {
		t.Errorf("--max-turns = %q, want 12", got)
	}
}
