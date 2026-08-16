package agent

// The mid-run half of fallback:. Preflight (preflight.go) picks a source
// before any request is made; this file reacts when the source actually
// running the conversation dies partway through it instead — attempts:
// exhausted on a transient failure — and resumes the SAME conversation on
// the next fallback: source rather than just failing the step.

import (
	"context"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

// runPreparedWithFailover runs prepared's conversation and, on a transient
// mid-run failure, cascades through the agent's remaining fallback: sources
// — resuming the same conversation on each, in order — until one finishes
// the step, a failure turns out not to be transient, or the list runs out.
//
// It is automatic: declaring fallback: already means "steps may run this
// agent on any of these sources", so no separate opt-in gates the mid-run
// trigger beyond the list being non-empty. It replaces the direct
// runPrepared call RunStep/RunHook used to make; the CLI-primary path and
// the plain single-source path both still happen exactly as before, since
// this only adds a cascade on top of what runPrepared already did.
//
// Scope: only HOSTED (HTTP) sources participate in the cascade, in both
// directions. A CLI-backed source already owns its own resume mechanism
// (session rejoin, see cli.go's own doc comment) and its own usage lifecycle
// (dollar-metered, and it finishes its own *stepUsage internally) — mixing
// that with this mechanism's token-accumulating, multi-source usage
// tracking is a correctness hazard, not a simplification. So: a CLI primary
// runs to its own conclusion with no mid-run cascade at all (fallback: still
// reaches a CLI source exactly as today, via preflight), and a hosted
// cascade that reaches a CLI candidate in agent.Fallback stops there rather
// than skipping over it — skipping an operator's explicitly ordered entry
// would be its own kind of surprise.
func runPreparedWithFailover(ctx context.Context, cfg *config.Config, prepared preparedAgentStep) (conversationResult, config.ResolvedInvocation, error) {
	timeout := agentTimeout(prepared.ri.Timeout)

	if prepared.ri.CLI != "" {
		res, err := runCLIConversation(ctx, prepared, timeout)

		return res, prepared.ri, err
	}

	agent, err := cfg.FindAgent(prepared.primary.AgentName)
	if err != nil {
		// Resolved moments ago by prepareAgentStep; not worth failing the
		// step over now. An empty agent just means the cascade below finds
		// no fallback: entries to advance through.
		agent = &config.Agent{} //nolint:exhaustruct // deliberately zero-value: only .Fallback (nil, empty) is read below
	}

	prepared.conv.usage = attachUsage(ctx, prepared.conv.usage)
	defer prepared.conv.usage.finish()

	ri := prepared.ri
	llm := prepared.llm
	conv := prepared.conv
	index := prepared.fallbackIndex

	for {
		res, runErr := runOneConversation(ctx, ri, llm, conv, timeout)
		if !failoverEligible(ctx, runErr) {
			return res, ri, runErr
		}

		next, nextIndex, ok := nextHostedFallback(agent, ri, index)
		if !ok {
			return res, ri, runErr
		}

		selectSource(agent.Name, agent.Fallback[nextIndex].Source)

		// Loud, not silent, matching preflight's own failover — a fallback
		// model can produce meaningfully different output, and a quality dip
		// caused by an outage must not look identical to a normal run.
		slog.Warn("agent.failover",
			"agent", agent.Name,
			"from", ri.ModelName,
			"to", next.ModelName,
			"reason", "attempts exhausted mid-run")

		apiKey, keyErr := lookupAPIKey(next.APIKeyEnv, next.RequiresKey)
		if keyErr != nil {
			return res, ri, runErr
		}

		conv.resumeContents = res.endContents
		// invocationLLM returns nil only for a CLI source (see its own doc
		// comment) — never reachable here, since nextHostedFallback already
		// filtered out any candidate whose .CLI is set. (nilaway flags this
		// call anyway: its interprocedural analysis doesn't correlate
		// nextHostedFallback's filter with invocationLLM's nil branch — a
		// known false-positive shape, triaged, not a live nil risk.)
		ri, llm, index = next, invocationLLM(next, apiKey), nextIndex
	}
}

// failoverEligible reports whether runErr is the class of mid-run failure
// the cascade should react to: a transient provider failure (a timeout, an
// unreachable endpoint, a 5xx — see isTransientProviderError) that is also
// classified as infrastructure (outcome.Errored), not a task-level outcome.
// A turn-exhausted or loop-detected attempt, or a canceled/deadline-exceeded
// context, is excluded by the outcome.Errored check alone — the same
// exclusion the docs already promise: "a model refusing a request is a
// different class entirely."
func failoverEligible(ctx context.Context, runErr error) bool {
	return runErr != nil && outcome.Classify(ctx, runErr) == outcome.Errored && isTransientProviderError(runErr)
}

// nextHostedFallback resolves the next candidate after index in agent's
// fallback: list, or reports false when there is none, or when it doesn't
// resolve to a hosted source — the mid-run cascade stops there rather than
// skipping over a declared entry (see runPreparedWithFailover's doc
// comment).
func nextHostedFallback(agent *config.Agent, ri config.ResolvedInvocation, index int) (config.ResolvedInvocation, int, bool) {
	nextIndex := index + 1
	if nextIndex >= len(agent.Fallback) {
		return config.ResolvedInvocation{}, 0, false
	}

	next, err := ri.WithSource(agent.Fallback[nextIndex].Source, agent)
	if err != nil || next.CLI != "" {
		return config.ResolvedInvocation{}, 0, false
	}

	return next, nextIndex, true
}
