package agent

import (
	"context"
	"slices"
	"testing"
	"time"
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
