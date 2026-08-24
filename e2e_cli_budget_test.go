package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// End-to-end coverage of a CLI agent's dollar budget across RETRIES.
//
// The budget is the only spending control a CLI agent has: the subprocess
// reports what it spent when it exits, far too late to stop anything, so the
// ceiling is handed to the child up front and enforced by it. A number that
// quietly meant "per attempt" would make the one real control unreliable in
// the one situation it exists for — three attempts against a $0.50 budget
// spending $1.50 — which is what these pin.
//
// They cannot be package-level parallel: installing a fake binary means
// editing PATH for the process.

// cliBudgetPipeline is one CLI agent with a dollar budget and three attempts.
func cliBudgetPipeline(t *testing.T, dir string, usd float64) string {
	t.Helper()

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: spender
  source:
    model: "@claude/sonnet"
  system: You spend.
  attempts: 3
  budget:
    usd: %v
  tools: [read_file]

jobs:
- name: spend
  plan:
  - agent: spender
    inputs: []
    messages:
      - Go.
`, usd)

	return writePipeline(t, dir, yaml)
}

// cliUsageOnlyRun is a child that streams one assistant turn's usage and then
// dies without a terminal result event — the exact shape the budget carry-over
// exists for, since a crashed attempt reports no total_cost_usd at all.
func cliUsageOnlyRun(inputTokens, outputTokens int) string {
	return fmt.Sprintf(
		`{"type":"assistant","message":{"content":[],"usage":{"input_tokens":%d,"output_tokens":%d}}}`,
		inputTokens, outputTokens)
}

// budgetArg reads the --max-budget-usd value out of a recorded argument
// vector. Arguments are recorded "|"-separated, so this reads a whole value
// rather than a substring of a flattened command line.
func budgetArg(t *testing.T, argv string) (float64, bool) {
	t.Helper()

	fields := strings.Split(argv, "|")
	for i, field := range fields {
		if field != "--max-budget-usd" || i+1 >= len(fields) {
			continue
		}

		usd, err := strconv.ParseFloat(fields[i+1], 64)
		if err != nil {
			t.Fatalf("--max-budget-usd = %q, which does not parse: %v", fields[i+1], err)
		}

		return usd, true
	}

	return 0, false
}

func TestE2ECLIAgentBudgetCarriesAcrossAttempts(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "first-attempt-done")

	// 50,000 input and 5,000 output tokens on sonnet is well under the $0.50
	// ceiling, so the retry gets a REDUCED purse rather than an exhausted one.
	cli := writeFakeClaude(t, fmt.Sprintf(`
if [ -f %[1]q ]; then
  echo '%[3]s'
else
  : > %[1]q
  echo '%[2]s'
  exit 1
fi
`, marker, cliUsageOnlyRun(50_000, 5_000), cliResultEvent("done", 2)))

	path := cliBudgetPipeline(t, dir, 0.50)

	_ = run([]string{path})

	if got := cli.invocations(t); got != 2 {
		t.Fatalf("the fake cli ran %d times, want 2 (a crash and its retry)", got)
	}

	first, ok := budgetArg(t, cli.argv(t, 1))
	if !ok {
		t.Fatal("the first attempt was given no --max-budget-usd")
	}

	if first != 0.50 {
		t.Errorf("first attempt's budget = %v, want the whole declared 0.50", first)
	}

	second, ok := budgetArg(t, cli.argv(t, 2))
	if !ok {
		t.Fatal("the retry was given no --max-budget-usd")
	}

	// The exact arithmetic is pinned in internal/agent; what matters here is
	// that the retry did not start over with a fresh purse.
	if second >= first {
		t.Errorf("retry's budget = %v, want less than the first attempt's %v — a retry must not restart the ceiling", second, first)
	}

	if second <= 0 {
		t.Errorf("retry's budget = %v, want what is LEFT of 0.50 after a partial attempt, not nothing", second)
	}
}

func TestE2ECLIAgentBudgetExhaustionFailsTheStep(t *testing.T) {
	dir := t.TempDir()

	// 100,000 input and 20,000 output on sonnet is $0.60 — more than the whole
	// ceiling, so there is nothing left to hand a second attempt.
	cli := writeFakeClaude(t, "echo '"+cliUsageOnlyRun(100_000, 20_000)+"'\nexit 1")

	path := cliBudgetPipeline(t, dir, 0.10)

	err := run([]string{path})
	if err == nil {
		t.Fatal("the job passed, want a failure naming the exhausted budget")
	}

	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("failure = %v, want it to name the exhausted budget", err)
	}

	// One attempt spent the lot; the remaining two are not worth paying for.
	if got := cli.invocations(t); got != 1 {
		t.Errorf("the fake cli ran %d times, want 1 — an exhausted budget stops the attempts", got)
	}
}
