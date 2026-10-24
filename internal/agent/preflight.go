package agent

// Preflight: prove a job's models and MCP servers work BEFORE any of its steps
// run.
//
// A pipeline would happily spend twenty minutes and real money before
// discovering that something it needed was never going to work. A model that
// was not serving, and an MCP server whose binary was not installed, were both
// found at the moment they were first used — which for a plan like
// plan -> code -> check -> review -> publish is half an hour in, with
// everything before it paid for and thrown away.
//
// Both are yes/no facts, checkable in seconds, before any work starts.
//
// What this does NOT do, stated plainly so nobody over-trusts it: preflight
// catches "broken before we start", not "breaks halfway through". In the
// incident that motivated it the model answered a test request at 08:10, the
// run started at 08:12 — a preflight there would have PASSED — and the first
// 500 arrived at 08:48. Failing over mid-run is a different feature — see
// failover.go's runPreparedWithFailover, which reacts to exactly that case.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/agent/llmclient"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

// transientErr marks a probe failure that WAITING could fix — a model that
// did not answer, a server that did not accept a connection — as opposed to
// one no amount of time changes: a tool the server does not expose, a
// credential that is not set, an oauth token only a human can renew.
//
// The distinction is carried on the error rather than decided from its text
// because only the site that produced it knows which kind it is, and it is
// only marked where that is certain. Everything unmarked is terminal, so a
// failure nobody classified refuses the run rather than being retried
// forever in a watcher — the safer direction to be wrong in.
//
// See config.Problem.Transient for what the two callers do with it, and why
// `steps run` and `steps web` want opposite reactions to the same fact.
type transientErr struct{ error }

func (t transientErr) Unwrap() error { return t.error }

// transient marks err as waitable, unless it is already an oauth failure
// that needs a human — a token that cannot be refreshed does not heal on the
// next poll, however much it looks like a connection problem from here.
func transient(err error) error {
	if errors.Is(err, stepsmcp.ErrNeedsLogin) {
		return err
	}

	return transientErr{err}
}

func isTransient(err error) bool {
	var marked transientErr

	return errors.As(err, &marked)
}

// Preflight checks every model and MCP server the named agents need, and
// returns a problem per target that failed. An empty result means the job's
// dependencies are all live.
//
// Only what this job needs: a pipeline with ten agents whose job uses two
// checks two. Distinct (endpoint, model) pairs are checked once even when
// several agents share one.
func Preflight(ctx context.Context, cfg *config.Config, agentNames []string, settings *config.Preflight) []config.Problem {
	var (
		problems []config.Problem
		// healthyEndpoints records which endpoints answered SOMETHING, so a
		// failure can say whether its neighbours on the same endpoint were
		// fine. That contrast is the diagnostic: "other models on this
		// endpoint responded" points at the model, not the credentials.
		healthyEndpoints = map[string]bool{}
		failures         []modelFailure
		seenServer       = map[string]bool{}
	)

	for _, name := range withSubAgents(cfg, agentNames) {
		agent, err := cfg.FindAgent(name)
		if err != nil || !preflightEnabled(agent, settings) {
			continue
		}

		// Resolved against the STEPS that run this agent, not against a bare
		// one. Container settings merge agent and step (resolveAgentRuntime),
		// so a step-level `image:` is invisible to a synthetic step — and a
		// CLI probe reads ri.Image to decide whether to look on this host's
		// PATH or inside the image. Getting that backwards checked the wrong
		// thing entirely.
		//
		// A pin belongs to an agent and says nothing about which invocation
		// asked, so it is reconsidered once per agent rather than once per
		// invocation. Two invocations of one agent can legitimately disagree
		// — a CLI reached both on this host and under an `image:` probes
		// separately (cliProbeKey) — and letting the second re-decide let it
		// undo the failover the first had just recorded.
		pinDecided, keepPin := false, false
		scope := agentPinScope(cfg, agent.Name)

		for _, ri := range agentInvocations(cfg, name) {
			// Every agent gets its own decision, even when several share a
			// model. Deduping by (endpoint, model) and skipping the later
			// agents left them with no failover recorded while preflight
			// reported zero problems — the run then died mid-plan against the
			// dead primary, which is exactly the failure preflight exists to
			// move earlier. probeModelCached already collapses the network
			// cost to one request.
			probeAt, probeErr := probeModelCached(ctx, ri, settings)

			// Before acting on the probe: a pin is durable state, and this is
			// the only boundary that reconsiders it.
			if !pinDecided {
				keepPin = reconsiderPin(ctx, scope, agent, ri, settings, probeAt, probeErr)
				pinDecided = true
			}

			// keepPin suppresses failOver, and has to: failOver re-selects
			// from fallback index 0 unconditionally, so an agent pinned
			// further down the list would be silently moved back to whichever
			// earlier entry answers a probe — abandoning a source that served
			// a whole conversation for one that answered a single token.
			if probeErr == nil {
				healthyEndpoints[ri.BaseURL] = true
			} else if !keepPin && !failOver(ctx, scope, agent, ri, settings) {
				failures = append(failures, modelFailure{name: name, ri: ri, err: probeErr})
			}

			problems = append(problems, probeAgentServers(ctx, cfg, ri, settings, seenServer)...)
		}
	}

	return append(problems, renderModelFailures(failures, healthyEndpoints)...)
}

// agentInvocations resolves one agent once per DISTINCT way a step runs it.
//
// Usually that is a single invocation: most agents are referenced by steps
// that override nothing about the runtime. It is more when steps disagree —
// one running the agent on the host and another under an image: — and those
// are genuinely different questions to ask about this machine, so each gets
// probed. Duplicates are collapsed here rather than left to the probe cache,
// so a job with twenty identical steps does not walk twenty invocations.
//
// An agent no step names (a sub-agent, or one preflighted by name alone)
// falls back to a bare step, which is what this always did.
func agentInvocations(cfg *config.Config, name string) []config.ResolvedInvocation {
	steps := cfg.StepsForAgent(name)
	if len(steps) == 0 {
		steps = []config.Step{{Agent: name}}
	}

	var (
		invocations []config.ResolvedInvocation
		seen        = map[string]bool{}
	)

	for _, step := range steps {
		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			continue // an unresolvable agent is already a load error
		}

		// Keyed on what a probe actually varies over. Two steps differing
		// only in prompt or inputs ask this machine the same question.
		key := ri.BaseURL + "|" + ri.ModelName + "|" + ri.CLI + "|" + ri.Image
		if seen[key] {
			continue
		}

		seen[key] = true

		invocations = append(invocations, ri)
	}

	return invocations
}

// withSubAgents expands a job's agent list to include the sub-agents those
// agents may delegate to. A sub-agent is a separate model with a separate
// credential and is just as capable of being down — and it is reached mid-run,
// which is exactly the moment preflight exists to move earlier.
func withSubAgents(cfg *config.Config, names []string) []string {
	out := append([]string(nil), names...)
	seen := map[string]bool{}

	for _, name := range out {
		seen[name] = true
	}

	// Ranges over out while appending to it, so a sub-agent's own sub-agents
	// are reached too. seen bounds it against a cycle.
	for i := 0; i < len(out); i++ {
		agent, err := cfg.FindAgent(out[i])
		if err != nil {
			continue
		}

		for _, spec := range agent.Tools {
			if spec.Agent != "" && !seen[spec.Agent] {
				seen[spec.Agent] = true
				out = append(out, spec.Agent)
			}
		}
	}

	return out
}

// failOver tries each of an agent's fallback sources in order and selects the
// first that answers, reporting whether one did.
//
// Preflight is the natural trigger: a primary that fails here is exactly when
// to pick an alternate — before the run has spent anything, rather than after
// failing partway in. It fires only on connection-level failures because that
// is all a probe can produce; a model REFUSING a request is a different class
// entirely, and falling over on one would silently reroute a legitimate
// refusal to a possibly less suitable model.
func failOver(ctx context.Context, scope pinScope, agent *config.Agent, primary config.ResolvedInvocation, settings *config.Preflight) bool {
	for i := range agent.Fallback {
		candidate, err := primary.WithSource(agent.Fallback[i].Source, agent)
		if err != nil {
			continue // an unresolvable fallback is already a load error
		}

		_, probeErr := probeModelCached(ctx, candidate, settings)
		if probeErr != nil {
			slog.WarnContext(ctx, "agent.fallback_unavailable",
				"pipeline", scope.pipeline,
				"agent", agent.Name, "fallback", i, "model", candidate.ModelName, "error", probeErr)

			continue
		}

		selectSource(scope, agent.Fallback[i].Source, i)

		// Loud, not silent. A fallback model can produce meaningfully
		// different output, and a quality dip caused by an outage must not
		// look identical to a normal run — otherwise nobody investigates.
		// Named by pipeline as well as agent: these lines fire with no run to
		// place them, and one process can be serving several pipelines that
		// each declare a `reviewer`. An operator watching for a change in
		// model quality cannot act on a line that does not say whose.
		slog.WarnContext(ctx, "agent.failover",
			"pipeline", scope.pipeline,
			"agent", agent.Name,
			"from", primary.ModelName,
			"to", candidate.ModelName,
			"reason", "the primary model did not answer preflight")

		return true
	}

	return false
}

// reconsiderPin re-decides, once per run, whether an agent should still be
// running on the fallback it was pinned to, and reports whether the pinned
// source is KNOWN to be serving.
//
// What the pin gains here is an exit condition informed by probes, from a fact
// preflight was already gathering on a schedule and discarding: the primary's
// own probe, cached under defaults.preflight.cache:. The other exit is the
// pin's own lifetime, which is what covers a run where none of this executes.
//
// The pinned source is probed too, because releasing on a recovered primary
// WITHOUT that would trade one blind spot for a worse one — preflight looks
// only at the primary, so a pinned fallback that dies is otherwise discovered
// by a step failing. Both facts feed one pure decision (llmclient.DecidePreflightPin).
//
// The reported bool is deliberately NARROWER than "the pin survived". It
// suppresses the caller's failover AND its failure record, so it may only mean
// the strong thing: this source answered a probe. A pin left standing because
// nothing fresh contradicted it is not the same claim — reporting that as a
// keep is how a job started green with its primary freshly dead and its
// pinned fallback dead in the cache, with no problem reported anywhere.
func reconsiderPin(
	ctx context.Context,
	scope pinScope,
	agent *config.Agent,
	primary config.ResolvedInvocation,
	settings *config.Preflight,
	primaryAt time.Time,
	primaryErr error,
) bool {
	// pinnedSource, not selectedSource: the lifetime is the answer for when
	// nothing is asking, and this function is asking. Letting the clock fire
	// here would delete a pin a real probe is about to re-decide better.
	record, pinned := pinnedSource(scope)
	if !pinned {
		return false
	}

	selection := record.selection

	// Fresh means the answer POSTDATES the pin, which is the only bar that
	// says anything: an answer older than the pin was given before the reason
	// for the pin existed, so it cannot speak to whether that reason is gone.
	//
	// Deliberately not "this preflight pass asked for it". Preflight runs once
	// per JOB, several agents legitimately share one endpoint and model, and
	// the probe cache collapses them to a single request — so a pass-boundary
	// test called an answer from moments earlier stale purely because another
	// agent's pass had been the one to ask. That agent could then never see a
	// fresh positive, never release, and — since keeping renews the lifetime —
	// never expire either: pinned to a paid fallback permanently, by ordering
	// alone.
	primaryProbe := llmclient.ProbeFact{Healthy: primaryErr == nil, Fresh: primaryAt.After(record.since)}

	candidate, err := primary.WithSource(selection.source, agent)
	if err != nil {
		// Unusable rather than unwell, and it has to say so: keeping this pin
		// fails every step of the run on the same resolution error, but
		// reporting it through the health path below would blame a probe that
		// was never sent and name the model it left as "".
		if clearSourceIf(scope, selection) == clearedPin {
			slog.WarnContext(ctx, "agent.failover.pin_lost",
				"pipeline", scope.pipeline,
				"agent", agent.Name,
				"error", err,
				"reason", "the pinned fallback no longer resolves; re-deciding from the primary")
		}

		return false
	}

	// Probed only where its answer can still change the verdict:
	// llmclient.DecidePreflightPin short-circuits on a primary that is FRESHLY healthy,
	// and this is a real request against a provider the agent is about to
	// stop using — one that can spend the whole ProbeTimeout if the fallback
	// is the thing hanging. A merely cached positive no longer short-circuits
	// it, which is the point: that answer cannot end a pin on its own, so the
	// other fact is worth having.
	pinnedProbe := llmclient.ProbeFact{}
	if !primaryProbe.Recovered() {
		at, err := probeModelCached(ctx, candidate, settings)
		pinnedProbe = llmclient.ProbeFact{Healthy: err == nil, Fresh: at.After(record.since)}
	}

	// A cancelled run learned nothing about either source: both probes fail
	// for a reason about the operator, not the endpoint. llmclient.DecideCascade calls
	// that sourceUnproven and leaves the pin alone; so does this. It reports
	// false rather than true so the caller still records the failure.
	if ctx.Err() != nil {
		return false
	}

	if llmclient.DecidePreflightPin(pinned, primaryProbe, pinnedProbe) != llmclient.DropPin {
		// Both halves have to hold to report a keep. renewPinIf fails when a
		// concurrent cascade or expiry has already dropped this pin, and
		// claiming to keep one that is gone left the caller neither failing
		// over nor recording a failure — a green preflight with a dead primary
		// and no pin at all. pinnedProbe.healthy is the other half: see the
		// doc comment on what this bool is allowed to mean.
		return renewPinIf(scope, selection) && pinnedProbe.Healthy
	}

	return releasePin(scope, agent, selection, primary, candidate, primaryProbe)
}

// releasePin performs reconsiderPin's drop half: the compare-and-delete and
// the announcement. It reports keepPin for its caller, which is true in
// exactly one case — the pin it meant to drop is no longer the pin that is
// there.
func releasePin(
	scope pinScope,
	agent *config.Agent,
	selection sourceSelection,
	primary, candidate config.ResolvedInvocation,
	primaryProbe llmclient.ProbeFact,
) bool {
	// Compare-and-delete, not delete: this decision spans a network probe, and
	// a concurrently-running job's mid-run cascade can pin a source that has
	// actually SERVED inside that window. A blind delete would discard it on a
	// reading taken before it existed.
	switch clearSourceIf(scope, selection) {
	case pinReplaced:
		// Another job's cascade pinned a source that has actually SERVED while
		// this pass was probing. Reporting no pin sends the caller into
		// failOver, which re-selects from fallback index 0 unconditionally —
		// so the guard would save the pin from a stale delete and the
		// fall-through would overwrite it a statement later.
		return true
	case pinAlreadyGone:
		return false
	case clearedPin:
	}

	// As loud as leaving was. agent.failover warns on the way out, and an
	// operator watching for model-quality changes needs both edges — a silent
	// return is how a quality shift gets attributed to the wrong change.
	//
	// recovered(), not healthy: a primary that is healthy only in the cache
	// cannot be the reason this pin ended, so saying so would report a
	// recovery nobody had probed for — the exact line the stale-positive
	// defect used to print.
	if primaryProbe.Recovered() {
		slog.Warn("agent.failover.returned",
			"pipeline", scope.pipeline,
			"agent", agent.Name,
			"from", candidate.ModelName,
			"to", primary.ModelName,
			"reason", "the primary model answered preflight again")

		return false
	}

	slog.Warn("agent.failover.pin_lost",
		"pipeline", scope.pipeline,
		"agent", agent.Name,
		"model", candidate.ModelName,
		"reason", "the pinned fallback stopped answering preflight; re-deciding from the primary")

	return false
}

// modelFailure is one model that did not answer, held until every model has
// been tried so the message can compare it against its neighbours.
type modelFailure struct {
	name string
	ri   config.ResolvedInvocation
	err  error
}

// renderModelFailures turns failed probes into problems, adding the
// same-endpoint contrast when there is one to draw.
func renderModelFailures(failures []modelFailure, healthyEndpoints map[string]bool) []config.Problem {
	problems := make([]config.Problem, 0, len(failures))

	for _, failure := range failures {
		detail := fmt.Sprintf("model %q: %v", failure.ri.ModelName, failure.err)

		if healthyEndpoints[failure.ri.BaseURL] {
			detail += "\n    (other models on this endpoint responded — the model itself looks unavailable, not the endpoint or the key)"
		}

		problems = append(problems, config.Problem{
			Target:    fmt.Sprintf("agent %q", failure.name),
			Detail:    detail,
			Transient: isTransient(failure.err),
		})
	}

	return problems
}

// preflightEnabled reports whether an agent participates. A per-agent opt-out
// exists for a model expected to be slow to wake — a cold local model would
// otherwise fail a probe that a real conversation would have waited out.
func preflightEnabled(agent *config.Agent, settings *config.Preflight) bool {
	if agent.Preflight != nil {
		return *agent.Preflight
	}

	return settings.Enabled()
}

// cliProbeKey is the probe cache key for a CLI target — the
// `cli|<cli>|<model>` shape docs/agents-internals.md documents. No Image
// component: the CLI is always a host subprocess now (see the issue #100
// design note in cliexec.go), whether or not the step's TOOLS are
// containerized, so image: no longer changes what probeCLI asks.
func cliProbeKey(ri config.ResolvedInvocation) string {
	return "cli|" + ri.CLI + "|" + ri.ModelName
}

// probeModel sends the smallest possible completion request and reports how it
// went. It goes through the same client construction a real step uses, so the
// endpoint, the model name, and the credential are all exercised — a probe
// that bypassed any of them would pass for a run that could not start.
func probeModel(ctx context.Context, ri config.ResolvedInvocation, timeout time.Duration) error {
	if ri.CLI != "" {
		return probeCLI(ctx, ri, timeout)
	}

	apiKey, err := llmclient.LookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Named for its agent, with no invoke_agent of its own: a probe is steps checking a source, not the agent working, and an unnamed chat span reads as an Unknown agent in Honeycomb.
	probeCtx = llmclient.WithInvocation(probeCtx, &llmclient.Invocation{Agent: ri.AgentName, Conversation: events.RunID(ctx)})

	started := time.Now()

	// One token of output is all this needs: it is asking "does this model
	// answer", not "does it answer well".
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "hi"}},
		}},
		Config: &genai.GenerateContentConfig{MaxOutputTokens: 1},
	}

	llm := llmclient.New(ri, apiKey)

	for _, respErr := range llm.GenerateContent(probeCtx, req, false) {
		if respErr != nil {
			// A model that answered badly, or not at all, is a fact about
			// this minute — an outage, a rate limit, a cold start. The
			// credential problems that are NOT are caught above, before any
			// request is sent.
			return transient(describeProbeError(probeCtx, respErr, timeout))
		}

		slog.DebugContext(ctx, "preflight.model_ok", "model", ri.ModelName, "elapsed", time.Since(started))

		return nil
	}

	return transient(errors.New("the model returned no response at all"))
}

// describeProbeError turns a raw client error into something a reader can act
// on. A bare `500 Internal Server Error` reads like a transient glitch, so the
// natural reaction is to RAISE retries — which makes it strictly worse. That
// is exactly what happened during the investigation this feature came from.
func describeProbeError(ctx context.Context, err error, timeout time.Duration) error {
	if ctx.Err() != nil {
		return fmt.Errorf("no response within %s", timeout)
	}

	return err
}
