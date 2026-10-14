package usage

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

// TestEstimateCostPricesEachInputRate is the reason the table carries three
// input rates rather than one. A cached conversation reports nearly all of its
// prompt under the cache columns, so pricing the whole prompt at the fresh
// rate over-charges by roughly 10x — enough to fail a step nowhere near its
// ceiling.
func TestEstimateCostPricesEachInputRate(t *testing.T) {
	t.Parallel()

	// sonnet: $3 in, $15 out, so a cache read is $0.30 and a 5m cache write
	// is $3.75, all per million tokens.
	fresh := EstimateCost("sonnet", Tokens{Input: 1_000_000})
	if !closeEnough(fresh, 3) {
		t.Errorf("1M fresh input tokens = $%v, want $3", fresh)
	}

	cached := EstimateCost("sonnet", Tokens{CacheRead: 1_000_000})
	if !closeEnough(cached, 0.30) {
		t.Errorf("1M cache-read tokens = $%v, want $0.30", cached)
	}

	written := EstimateCost("sonnet", Tokens{CacheWrite: 1_000_000})
	if !closeEnough(written, 3.75) {
		t.Errorf("1M cache-creation tokens = $%v, want $3.75", written)
	}

	out := EstimateCost("sonnet", Tokens{Output: 1_000_000})
	if !closeEnough(out, 15) {
		t.Errorf("1M output tokens = $%v, want $15", out)
	}

	// The four add up, which is what a real attempt looks like.
	mixed := EstimateCost("sonnet", Tokens{
		Input:      50_000,
		CacheRead:  100_000,
		CacheWrite: 10_000,
		Output:     5_000,
	})
	if want := 0.15 + 0.03 + 0.0375 + 0.075; !closeEnough(mixed, want) {
		t.Errorf("a mixed attempt = $%v, want $%v", mixed, want)
	}
}

// TestEstimateCostKeysByModel covers the two spellings that actually reach
// --model, plus the deliberate residual gap.
func TestEstimateCostKeysByModel(t *testing.T) {
	t.Parallel()

	spent := Tokens{Output: 1_000_000}

	// "@claude/sonnet" reaches the child as the bare alias; a pinned source
	// reaches it as the full id. Both are the same model at the same rate.
	alias := EstimateCost("sonnet", spent)

	full := EstimateCost("claude-sonnet-5", spent)
	if !closeEnough(alias, full) {
		t.Errorf("alias priced at $%v and full id at $%v; they are one model", alias, full)
	}

	// A dated snapshot prices as its family rather than falling off the table.
	snapshot := EstimateCost("claude-sonnet-5-20260101", spent)
	if !closeEnough(snapshot, full) {
		t.Errorf("a dated snapshot priced at $%v, want its family's $%v", snapshot, full)
	}

	// Opus and haiku are not sonnet, so a single shared rate would pass every
	// assertion above and still be wrong.
	if opus := EstimateCost("opus", spent); !closeEnough(opus, 25) {
		t.Errorf("1M opus output tokens = $%v, want $25", opus)
	}

	if haiku := EstimateCost("haiku", spent); !closeEnough(haiku, 5) {
		t.Errorf("1M haiku output tokens = $%v, want $5", haiku)
	}

	// The residual gap, deliberate: a model with no rate card debits nothing,
	// which is how every model behaved before this table existed. Guessing a
	// rate would fail steps for a number nobody wrote down.
	if got := EstimateCost("some-local-model", spent); got != 0 {
		t.Errorf("an unpriced model estimated $%v, want $0 rather than a guess", got)
	}
}
