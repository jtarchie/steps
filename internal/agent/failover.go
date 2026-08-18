package agent

// The mid-run half of fallback:. Preflight (preflight.go) picks a source
// before any request is made; this file reacts when the source actually
// running the conversation dies partway through it instead — attempts:
// exhausted on a transient failure — and resumes the SAME conversation on
// the next fallback: source rather than just failing the step.

import (
	"context"
	"log/slog"

	"google.golang.org/adk/v2/model"

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
func runPreparedWithFailover(ctx context.Context, prepared preparedAgentStep) (conversationResult, servedSource, error) {
	timeout := agentTimeout(prepared.ri.Timeout)

	if prepared.ri.CLI != "" {
		res, err := runCLIConversation(ctx, prepared, timeout)

		return res, servedSource{ri: prepared.ri, llm: prepared.llm}, err
	}

	// timeout: bounds the STEP, not each source it is tried on. The deadline
	// is established once, here, and every source below runs under it: a
	// cascade of three sources under `timeout: 10m` has ten minutes between
	// them, not thirty.
	//
	// runOneConversation is therefore passed noAgentDeadline rather than the
	// duration again. It used to re-apply it, which allocated a second timer
	// per source whose deadline — being set later — could never fire first,
	// and left a reader deriving from context.WithTimeout's min-deadline
	// semantics that the inner one was inert.
	//
	// Classification still reads the JOB's context, not this one (see
	// classifySource): a canceled job is an abort, while this context
	// expiring is the step spending its own budget.
	cascadeCtx, cancel := withAgentDeadline(ctx, timeout)
	defer cancel()

	prepared.conv.usage = attachUsage(ctx, prepared.conv.usage)
	defer prepared.conv.usage.finish()

	agent := prepared.agent
	ri := prepared.ri
	llm := prepared.llm
	conv := prepared.conv
	index := prepared.fallbackIndex
	swapped := false

	for {
		res, runErr := runOneConversation(cascadeCtx, ri, llm, conv, noAgentDeadline)

		// A cascade is a search for a source that can finish the step within
		// the step's own deadline, so both facts the decision needs beyond
		// the error itself are gathered before it is made. Resolving the next
		// candidate up front costs nothing: it is a config walk and an
		// environment lookup, no request.
		next, apiKey, nextIndex, hasNext := nextViableFallback(agent, ri, index)

		verdict := decideCascade(classifySource(ctx, runErr), hasNext, cascadeCtx.Err() != nil, swapped)

		switch verdict.pin {
		case pinThisSource:
			pinServedSource(agent, index)
		case dropPin:
			releaseSource(agent)
		case leavePin:
		}

		if verdict.action == returnResult {
			// Marked here, after decideCascade: the cascade reasons about the
			// spent deadline as an Errored source-outcome, but what leaves the
			// step is a task-level failure — the step outlived its timeout:.
			return res, servedSource{ri: ri, llm: llm, swapped: swapped}, outcome.FailOnDeadline(ctx, cascadeCtx, runErr) //nolint:wrapcheck // classification wrapper; the step caller adds its own context
		}

		// Loud, not silent, matching preflight's own failover — a fallback
		// model can produce meaningfully different output, and a quality dip
		// caused by an outage must not look identical to a normal run.
		//
		// live is read from the recorder RunStep/RunHook/RunFix already set
		// up (see step.go's own doc comment on prepared.conv.recorder) rather
		// than threaded as parameters here — the same reason
		// executeBudgetedTool reads it: this line otherwise names the two
		// models and nothing about which job, step, or run hit the cascade.
		live := prepared.conv.recorder.liveIdentity()

		slog.Warn("agent.failover",
			"run", live.runID,
			"job", live.job,
			"step", live.stepName,
			"index", live.stepIndex,
			"agent", agent.Name,
			"from", ri.ModelName,
			"to", next.ModelName,
			"reason", "attempts exhausted mid-run")

		// Resume the SAME conversation on the new source rather than
		// restarting it — see resumeCheckpoint for everything that has to
		// travel for that to be true, and what each omission used to cost.
		checkpoint := res.checkpoint
		conv.resume = &checkpoint
		// The dials WithSource just resolved for the NEW source — its own
		// tool_choice encoding and its own compaction budget (see
		// config.ResolvedInvocation.WithSource) — travel with it: leaving
		// these at the primary's values sent the wrong tool_choice shape to
		// a fallback that needed the other one, and compacted (or failed to)
		// a resumed conversation against the wrong model's context window.
		conv.toolChoiceStringOnly = next.StringOnlyToolChoice
		conv.compactAfterTokens = next.CompactAfterTokens

		// invocationLLM returns nil only for a CLI source (see its own doc
		// comment) — never reachable here, since nextHostedFallback already
		// filtered out any candidate whose .CLI is set. (nilaway flags this
		// call anyway: its interprocedural analysis doesn't correlate
		// nextHostedFallback's filter with invocationLLM's nil branch — a
		// known false-positive shape, triaged, not a live nil risk.)
		ri, llm, index, swapped = next, invocationLLM(next, apiKey), nextIndex, true
	}
}

// servedSource is which source actually ran a conversation, and how it was
// reached.
//
// swapped reports that the mid-run cascade moved off the source the step
// STARTED on. It is a fact about the cascade rather than a comparison of
// model names: two fallback: entries may legitimately name the same model and
// endpoint (a key rotation, say), and diffing their names would then report
// no swap at all — silencing the step's own visible notice while the
// agent.failover log line still fired.
type servedSource struct {
	ri      config.ResolvedInvocation
	llm     model.LLM
	swapped bool
}

// pinServedSource records that a fallback entry served a run, so the rest of
// the process prefers it over a primary that is evidently unwell. index -1
// means the step ran on the primary, which is the default and needs no pin.
func pinServedSource(agent *config.Agent, index int) {
	if index < 0 || index >= len(agent.Fallback) {
		return
	}

	selectSource(agent.Name, agent.Fallback[index].Source, index)
}

// releaseSource drops any pin after a cascade in which nothing served.
func releaseSource(agent *config.Agent) {
	clearSource(agent.Name)
}

// nextViableFallback walks agent.Fallback strictly forward from index,
// resolving the next candidate whose source is valid AND whose credential is
// actually present in this environment, skipping past anything else in
// between — a config problem on one entry (an unresolvable source, a missing
// api_key_env) must not wall off a healthy entry further down the same
// ordered list, the same "try the next one" treatment preflight's own
// failOver already gives an unhealthy candidate (preflight.go). It reports
// false once nextHostedFallback runs out of candidates to try, which also
// covers a CLI-typed candidate — see nextHostedFallback's own doc comment
// for why that one stops the walk rather than being skipped past.
func nextViableFallback(agent *config.Agent, ri config.ResolvedInvocation, index int) (config.ResolvedInvocation, string, int, bool) {
	for {
		next, nextIndex, ok := nextHostedFallback(agent, ri, index)
		if !ok {
			return config.ResolvedInvocation{}, "", 0, false
		}

		apiKey, err := lookupAPIKey(next.APIKeyEnv, next.RequiresKey)
		if err == nil {
			return next, apiKey, nextIndex, true
		}

		index = nextIndex
	}
}

// nextHostedFallback resolves the next candidate after index in agent's
// fallback: list whose source actually resolves, skipping any entry
// WithSource rejects (an unresolvable fallback is already a load error, so
// there is nothing to gain by giving up on the whole list over one bad
// entry — matching preflight's own failOver, which does the same). It
// reports false when no such candidate remains, or when the next resolvable
// one is CLI-backed: a hosted cascade stops there rather than skipping over
// a declared entry (see runPreparedWithFailover's doc comment) — skipping an
// operator's explicitly ordered CLI entry to reach a hosted one further down
// would be its own kind of surprise.
func nextHostedFallback(agent *config.Agent, ri config.ResolvedInvocation, index int) (config.ResolvedInvocation, int, bool) {
	for nextIndex := index + 1; nextIndex < len(agent.Fallback); nextIndex++ {
		next, err := ri.WithSource(agent.Fallback[nextIndex].Source, agent)
		if err != nil {
			continue // an unresolvable fallback is already a load error; try the next one
		}

		if next.CLI != "" {
			return config.ResolvedInvocation{}, 0, false
		}

		return next, nextIndex, true
	}

	return config.ResolvedInvocation{}, 0, false
}
