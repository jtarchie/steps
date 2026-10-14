package agent

import (
	"log/slog"

	"github.com/jtarchie/steps/internal/usage"
)

// estimateCLICost prices what a CLI run streamed, for a run that never
// reported a figure of its own.
func estimateCLICost(model string, spent cliUsage) float64 {
	return usage.EstimateCost(model, usage.Tokens{
		Input:      spent.InputTokens,
		CacheRead:  spent.CacheReadInputTokens,
		CacheWrite: spent.CacheCreationInputTokens,
		Output:     spent.OutputTokens,
	})
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
