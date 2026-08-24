package agent

import (
	"math"
	"testing"
)

// closeEnough compares dollar figures without asking binary floats to be
// exact: every rate here is a decimal fraction of a million, so the arithmetic
// is inexact by construction and a hundredth of a cent is far tighter than any
// budget decision made from it.
func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-6
}

// TestRemainingCLIBudgetCarriesTheSentinel is the dollar twin of
// TestRemainingCLITurnsCarriesTheSentinel, and the collision it guards is the
// sharper of the two: 0 is already how "no budget" is spelled on the way into
// cliArgs, so an exhausted ceiling must never arrive there as 0.
func TestRemainingCLIBudgetCarriesTheSentinel(t *testing.T) {
	t.Parallel()

	if got := remainingCLIBudget(0, 1.23); got != unlimitedBudget {
		t.Errorf("remainingCLIBudget(0, 1.23) = %v, want the unlimited sentinel", got)
	}

	if got := remainingCLIBudget(0.50, 0.20); !closeEnough(got, 0.30) {
		t.Errorf("remainingCLIBudget(0.50, 0.20) = %v, want 0.30", got)
	}

	// A CLI's own circuit breaker trips mid-conversation and the call in
	// flight is still paid for, so a step can legitimately come back having
	// spent more than it was given.
	overspent := remainingCLIBudget(0.50, 0.75)
	if overspent == unlimitedBudget {
		t.Fatal("an overspent budget collided with the unlimited sentinel")
	}

	if !(cliAttempt{budgetUSD: overspent}).outOfBudget() {
		t.Errorf("remainingCLIBudget(0.50, 0.75) = %v, which does not read as an exhausted budget", overspent)
	}

	if (cliAttempt{budgetUSD: unlimitedBudget}).outOfBudget() {
		t.Error("a step that declared no budget read as having exhausted one")
	}
}

// TestEstimateCLICostPricesEachInputRate is the reason the table carries three
// input rates rather than one. A cached conversation reports nearly all of its
// prompt under the cache columns, so pricing the whole prompt at the fresh
// rate over-charges by roughly 10x — enough to fail a step nowhere near its
// ceiling.
func TestEstimateCLICostPricesEachInputRate(t *testing.T) {
	t.Parallel()

	// sonnet: $3 in, $15 out, so a cache read is $0.30 and a 5m cache write
	// is $3.75, all per million tokens.
	fresh := estimateCLICost("sonnet", cliUsage{InputTokens: 1_000_000})
	if !closeEnough(fresh, 3) {
		t.Errorf("1M fresh input tokens = $%v, want $3", fresh)
	}

	cached := estimateCLICost("sonnet", cliUsage{CacheReadInputTokens: 1_000_000})
	if !closeEnough(cached, 0.30) {
		t.Errorf("1M cache-read tokens = $%v, want $0.30", cached)
	}

	written := estimateCLICost("sonnet", cliUsage{CacheCreationInputTokens: 1_000_000})
	if !closeEnough(written, 3.75) {
		t.Errorf("1M cache-creation tokens = $%v, want $3.75", written)
	}

	out := estimateCLICost("sonnet", cliUsage{OutputTokens: 1_000_000})
	if !closeEnough(out, 15) {
		t.Errorf("1M output tokens = $%v, want $15", out)
	}

	// The four add up, which is what a real attempt looks like.
	mixed := estimateCLICost("sonnet", cliUsage{
		InputTokens:              50_000,
		CacheReadInputTokens:     100_000,
		CacheCreationInputTokens: 10_000,
		OutputTokens:             5_000,
	})
	if want := 0.15 + 0.03 + 0.0375 + 0.075; !closeEnough(mixed, want) {
		t.Errorf("a mixed attempt = $%v, want $%v", mixed, want)
	}
}

// TestEstimateCLICostKeysByModel covers the two spellings that actually reach
// --model, plus the deliberate residual gap.
func TestEstimateCLICostKeysByModel(t *testing.T) {
	t.Parallel()

	usage := cliUsage{OutputTokens: 1_000_000}

	// "@claude/sonnet" reaches the child as the bare alias; a pinned source
	// reaches it as the full id. Both are the same model at the same rate.
	alias := estimateCLICost("sonnet", usage)

	full := estimateCLICost("claude-sonnet-5", usage)
	if !closeEnough(alias, full) {
		t.Errorf("alias priced at $%v and full id at $%v; they are one model", alias, full)
	}

	// A dated snapshot prices as its family rather than falling off the table.
	snapshot := estimateCLICost("claude-sonnet-5-20260101", usage)
	if !closeEnough(snapshot, full) {
		t.Errorf("a dated snapshot priced at $%v, want its family's $%v", snapshot, full)
	}

	// Opus and haiku are not sonnet, so a single shared rate would pass every
	// assertion above and still be wrong.
	if opus := estimateCLICost("opus", usage); !closeEnough(opus, 25) {
		t.Errorf("1M opus output tokens = $%v, want $25", opus)
	}

	if haiku := estimateCLICost("haiku", usage); !closeEnough(haiku, 5) {
		t.Errorf("1M haiku output tokens = $%v, want $5", haiku)
	}

	// The residual gap, deliberate: a model with no rate card debits nothing,
	// which is how every model behaved before this table existed. Guessing a
	// rate would fail steps for a number nobody wrote down.
	if got := estimateCLICost("some-local-model", usage); got != 0 {
		t.Errorf("an unpriced model estimated $%v, want $0 rather than a guess", got)
	}
}

// TestAttemptCostPrefersTheReportedFigure pins which of the two sources wins.
// The CLI's own total is authoritative — it covers whatever the vendor
// actually charged and needs no rate card — so the estimate is strictly a
// fallback for the attempt that died before reporting one.
func TestAttemptCostPrefersTheReportedFigure(t *testing.T) {
	t.Parallel()

	// A run that finished reports both; the reported figure wins even though
	// the streamed usage would price differently.
	finished := cliRunResult{
		sawResult: true,
		costUSD:   0.42,
		streamed:  cliUsage{InputTokens: 1_000_000},
	}
	if got := attemptCost("sonnet", finished); !closeEnough(got, 0.42) {
		t.Errorf("a finished attempt cost $%v, want the reported $0.42", got)
	}

	// A run that died reports nothing, which is the whole problem: subtracting
	// what it reported would hand the retry the full purse again.
	crashed := cliRunResult{streamed: cliUsage{InputTokens: 50_000, OutputTokens: 5_000}}

	got := attemptCost("sonnet", crashed)
	if want := 0.15 + 0.075; !closeEnough(got, want) {
		t.Errorf("a crashed attempt cost $%v, want the streamed usage priced at $%v", got, want)
	}

	if got <= 0 {
		t.Error("a crashed attempt that streamed usage was debited nothing")
	}
}
