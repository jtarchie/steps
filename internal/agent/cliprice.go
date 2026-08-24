package agent

import (
	"log/slog"
	"strings"
)

// Pricing a CLI attempt that died before it could say what it spent.
//
// This is the only place in steps that turns tokens into dollars, and it is
// deliberately narrow. Cost elsewhere is what a PROVIDER reported and nothing
// else — store.Usage.CostUSD is a pointer precisely so "nobody priced this"
// stays distinguishable from "this was free" (see usage.go), and every HTTP
// path reports no price at all. Nothing here changes that: an estimate made
// here is used only to decide how much of a step's dollar budget is left, and
// never to populate a recorded figure.
//
// The reason it has to exist is that the figure it replaces is missing exactly
// when it matters. A CLI reports total_cost_usd on its terminal result event,
// so an attempt that CRASHED — the case a per-step budget exists to bound —
// reports nothing at all, and subtracting what was reported would hand the
// retry a full purse again.

// cliRate is one model's list price, in dollars per million tokens.
//
// Three input rates, not one, because a cached conversation is billed almost
// entirely under the cache columns: pricing prompt() at the fresh-input rate
// over-charges by roughly 10x on a run that read its prompt from cache (see
// the 9-vs-21560 observation in clistream.go), which would fail steps nowhere
// near their ceiling. Cache creation is priced at the 5-minute TTL, which is
// the CLI's default; a 1-hour write costs more and is under-counted here.
type cliRate struct {
	input     float64
	output    float64
	cacheRead float64
	// cacheWrite is the 5-minute-TTL write rate.
	cacheWrite float64
}

// anthropicRate builds the three input rates from a model's base input price,
// which is how Anthropic's card expresses them: a cache read is 0.1x base and
// a 5-minute cache write is 1.25x.
func anthropicRate(input, output float64) cliRate {
	return cliRate{
		input:      input,
		output:     output,
		cacheRead:  input * 0.1,
		cacheWrite: input * 1.25,
	}
}

// cliRates prices the models the CLI runtimes actually run, keyed by the
// string handed to --model. Both spellings are listed because both are passed
// through untouched: "@claude/sonnet" reaches the child as `sonnet`, and a
// pinned "@claude/claude-sonnet-5" reaches it as the full id.
//
// A model missing from this table debits NOTHING for an unaccounted crash, and
// says so — see estimateCLICost. That is the deliberate residual gap: the
// ceiling still holds for every priced model, and an unpriced one behaves the
// way every model did before this table existed.
//
//nolint:gochecknoglobals // static, read-only lookup table
var cliRates = map[string]cliRate{
	"opus":              anthropicRate(5, 25),
	"claude-opus-5":     anthropicRate(5, 25),
	"claude-opus-4-8":   anthropicRate(5, 25),
	"claude-opus-4-7":   anthropicRate(5, 25),
	"claude-opus-4-6":   anthropicRate(5, 25),
	"sonnet":            anthropicRate(3, 15),
	"claude-sonnet-5":   anthropicRate(3, 15),
	"claude-sonnet-4-6": anthropicRate(3, 15),
	"haiku":             anthropicRate(1, 5),
	"claude-haiku-4-5":  anthropicRate(1, 5),
}

// estimateCLICost prices what a run streamed, for a run that never reported a
// figure of its own. Returns 0 for a model this build has no rate card for.
func estimateCLICost(model string, usage cliUsage) float64 {
	rate, priced := cliRates[cliRateKey(model)]
	if !priced {
		return 0
	}

	const perMillion = 1_000_000

	return (float64(usage.InputTokens)*rate.input +
		float64(usage.CacheReadInputTokens)*rate.cacheRead +
		float64(usage.CacheCreationInputTokens)*rate.cacheWrite +
		float64(usage.OutputTokens)*rate.output) / perMillion
}

// cliRateKey normalizes a --model value enough to match the table. A dated
// snapshot (claude-sonnet-5-20260101) prices as its family, since a snapshot
// and its alias are the same model at the same rate.
func cliRateKey(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	if _, known := cliRates[key]; known {
		return key
	}

	for name := range cliRates {
		if strings.HasPrefix(key, name+"-") {
			return name
		}
	}

	return key
}

// attemptCost is what one attempt spent, for the purpose of the step's dollar
// budget.
//
// The CLI's own figure wins whenever there is one: it is authoritative, it
// covers whatever the vendor actually charged, and it needs no rate card. The
// estimate is the fallback for the crash case, and only there.
func attemptCost(model string, run cliRunResult) float64 {
	if run.costUSD > 0 {
		return run.costUSD
	}

	estimate := estimateCLICost(model, run.streamed)
	if estimate == 0 && run.streamed.prompt()+run.streamed.OutputTokens > 0 {
		slog.Warn("agent.cli.unpriced_attempt",
			"model", model,
			"prompt_tokens", run.streamed.prompt(),
			"output_tokens", run.streamed.OutputTokens,
			"reason", "no rate card for this model; its spend is not debited from the step's budget")
	}

	return estimate
}
