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
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
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
// `steps run` and `steps watch` want opposite reactions to the same fact.
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

// probeCache remembers what has already been verified in this process, so a
// long-lived `steps watch` checks occasionally rather than on every poll. A
// process-wide cache rather than a per-run one for exactly that reason.
//
//nolint:gochecknoglobals // process-lifetime memo, deliberately shared across runs
var probeCache = &resultCache{entries: map[string]cacheEntry{}}

type cacheEntry struct {
	at  time.Time
	err error
}

type resultCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

// lookup returns a cached result when one is still within ttl.
func (c *resultCache) lookup(key string, ttl time.Duration, now time.Time) (found bool, result error) {
	if ttl <= 0 {
		return false, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) > ttl {
		return false, nil
	}

	return true, entry.err
}

func (c *resultCache) store(key string, err error, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{at: now, err: err}
}

// ResetProbeCache clears everything preflight has verified AND every source
// pin it (or a mid-run cascade) selected. Tests use it to stay independent of
// each other; a test about the pin's own lifetime wants only the first half,
// since wiping the pin is what such a test is trying to observe.
//
// Every caller must be a serial test: this is process-wide state for all
// agents, so a `t.Parallel()` test that reached it would clear another's.
func ResetProbeCache() {
	probeCache.mu.Lock()
	probeCache.entries = map[string]cacheEntry{}
	probeCache.mu.Unlock()

	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	selectedSources.by = map[string]sourceSelection{}
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
		// A pin is process-wide and says nothing about which invocation asked,
		// so it is reconsidered once per agent rather than once per
		// invocation. Two invocations of one agent can legitimately disagree
		// — a CLI reached both on this host and under an `image:` probes
		// separately (cliProbeKey) — and letting the second re-decide let it
		// undo the failover the first had just recorded.
		pinDecided, keepPin := false, false

		for _, ri := range agentInvocations(cfg, name) {
			// Every agent gets its own decision, even when several share a
			// model. Deduping by (endpoint, model) and skipping the later
			// agents left them with no failover recorded while preflight
			// reported zero problems — the run then died mid-plan against the
			// dead primary, which is exactly the failure preflight exists to
			// move earlier. probeModelCached already collapses the network
			// cost to one request.
			probeErr := probeModelCached(ctx, ri, settings)

			// Before acting on the probe: a pin is durable state, and this is
			// the only boundary that reconsiders it.
			if !pinDecided {
				keepPin = reconsiderPin(ctx, agent, ri, settings, probeErr == nil)
				pinDecided = true
			}

			// keepPin suppresses failOver, and has to: failOver re-selects
			// from fallback index 0 unconditionally, so an agent pinned
			// further down the list would be silently moved back to whichever
			// earlier entry answers a probe — abandoning a source that served
			// a whole conversation for one that answered a single token.
			if probeErr == nil {
				healthyEndpoints[ri.BaseURL] = true
			} else if !keepPin && !failOver(ctx, agent, ri, settings) {
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
func failOver(ctx context.Context, agent *config.Agent, primary config.ResolvedInvocation, settings *config.Preflight) bool {
	for i := range agent.Fallback {
		candidate, err := primary.WithSource(agent.Fallback[i].Source, agent)
		if err != nil {
			continue // an unresolvable fallback is already a load error
		}

		probeErr := probeModelCached(ctx, candidate, settings)
		if probeErr != nil {
			slog.Warn("agent.fallback_unavailable",
				"agent", agent.Name, "fallback", i, "model", candidate.ModelName, "error", probeErr)

			continue
		}

		selectSource(agent.Name, agent.Fallback[i].Source, i)

		// Loud, not silent. A fallback model can produce meaningfully
		// different output, and a quality dip caused by an outage must not
		// look identical to a normal run — otherwise nobody investigates.
		slog.Warn("agent.failover",
			"agent", agent.Name,
			"from", primary.ModelName,
			"to", candidate.ModelName,
			"reason", "the primary model did not answer preflight")

		return true
	}

	return false
}

// reconsiderPin re-decides, once per run, whether an agent should still be
// running on the fallback it was pinned to, and reports whether the pin was
// deliberately KEPT.
//
// The pin's SCOPE is unchanged — it still lasts for the life of the process.
// What it gains is an exit condition, from a fact preflight was already
// gathering on a schedule and discarding: the primary's own probe, cached
// under defaults.preflight.cache:.
//
// The pinned source is probed too, because releasing on a recovered primary
// WITHOUT that would trade one blind spot for a worse one — preflight looks
// only at the primary, so a pinned fallback that dies is otherwise discovered
// by a step failing. Both facts feed one pure decision (decidePreflightPin).
//
// The reported bool matters as much as the mutation. Dropping a pin is not
// choosing a source, so the caller falls through to its ordinary failover
// path; KEEPING one has to stop that path, since failOver re-selects from
// fallback index 0 whatever the pin says.
func reconsiderPin(
	ctx context.Context,
	agent *config.Agent,
	primary config.ResolvedInvocation,
	settings *config.Preflight,
	primaryHealthy bool,
) bool {
	selection, pinned := selectedSource(agent.Name)
	if !pinned {
		return false
	}

	candidate, err := primary.WithSource(selection.source, agent)
	if err != nil {
		// Unusable rather than unwell, and it has to say so: keeping this pin
		// fails every step of the run on the same resolution error, but
		// reporting it through the health path below would blame a probe that
		// was never sent and name the model it left as "".
		if clearSourceIf(agent.Name, selection) {
			slog.Warn("agent.failover.pin_lost",
				"agent", agent.Name,
				"error", err,
				"reason", "the pinned fallback no longer resolves; re-deciding from the primary")
		}

		return false
	}

	// Probed only where its answer can still change the verdict:
	// decidePreflightPin short-circuits on primaryHealthy, and this is a real
	// request against a provider the agent is about to stop using — one that
	// can spend the whole ProbeTimeout if the fallback is the thing hanging.
	pinnedHealthy := false
	if !primaryHealthy {
		pinnedHealthy = probeModelCached(ctx, candidate, settings) == nil
	}

	// A cancelled run learned nothing about either source: both probes fail
	// for a reason about the operator, not the endpoint. decideCascade calls
	// that sourceUnproven and leaves the pin alone; so does this. It reports
	// false rather than true so the caller still records the failure.
	if ctx.Err() != nil {
		return false
	}

	if decidePreflightPin(pinned, primaryHealthy, pinnedHealthy) != dropPin {
		return true
	}

	// Compare-and-delete, not delete: this decision spans a network probe, and
	// a concurrently-running job's mid-run cascade can pin a source that has
	// actually SERVED inside that window. A blind delete would discard it on a
	// reading taken before it existed.
	if !clearSourceIf(agent.Name, selection) {
		return false
	}

	// As loud as leaving was. agent.failover warns on the way out, and an
	// operator watching for model-quality changes needs both edges — a silent
	// return is how a quality shift gets attributed to the wrong change.
	if primaryHealthy {
		slog.Warn("agent.failover.returned",
			"agent", agent.Name,
			"from", candidate.ModelName,
			"to", primary.ModelName,
			"reason", "the primary model answered preflight again")

		return false
	}

	slog.Warn("agent.failover.pin_lost",
		"agent", agent.Name,
		"model", candidate.ModelName,
		"reason", "the pinned fallback stopped answering preflight; re-deciding from the primary")

	return false
}

// sourceSelection is which fallback: entry an agent is running on: the source
// itself, and its POSITION in the agent's fallback list.
//
// The position is stored rather than re-derived by searching the list for a
// matching source, because two entries may legitimately hold the same source
// (the same model reached through two entries that differ elsewhere, or an
// honest copy-paste). A value scan then reports the FIRST match, and a
// cascade continuing from that index re-tries the very source whose failure
// triggered it.
type sourceSelection struct {
	source config.AgentSource
	index  int
}

// selectedSources records which fallback an agent is running on, for the life
// of the process. Process-scoped like probeCache and for the same reason: a
// `steps watch` that failed over should stay failed over rather than
// re-failing-over on every poll. reconsiderPin is its one exit condition.
//
// Keyed by agent NAME alone, which is a real limit rather than a decision: one
// process can serve several pipelines (`steps web app.yml infra.yml`), and two
// pipelines that both declare an agent called `reviewer` share this entry.
//
//nolint:gochecknoglobals // process-lifetime selection, deliberately shared across runs
var selectedSources = struct {
	mu sync.Mutex
	by map[string]sourceSelection
}{by: map[string]sourceSelection{}}

// selectSource pins an agent to the fallback entry at index for the rest of
// the process.
//
// Callers pin a source that has PROVED itself — answered a preflight probe,
// or served a conversation through to its own conclusion — never one merely
// chosen. Pinning on selection alone strands the process on a source that
// never answered until reconsiderPin's next probe notices, which is a whole
// run's worth of steps aimed at something known not to work.
func selectSource(agentName string, source config.AgentSource, index int) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	selectedSources.by[agentName] = sourceSelection{source: source, index: index}
}

// clearSource drops an agent's pin, so the next run resolves from its primary
// again. Called when a cascade tried every source and none served: continuing
// to prefer the last one tried would be preferring a source that just failed.
// reconsiderPin drops a pin for its own reasons, through clearSourceIf.
func clearSource(agentName string) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	delete(selectedSources.by, agentName)
}

// clearSourceIf drops an agent's pin only while it still holds the selection
// the caller evaluated, reporting whether it did.
//
// The unconditional clearSource is right for the mid-run cascade, whose read
// and write are the same step of the same conversation. It is wrong for a
// decision that spans network probes: `steps watch --max-concurrent` and a
// multi-pipeline `steps web` both run jobs as concurrent goroutines against
// this one map, so another job's cascade can pin a source that has actually
// SERVED while this one is still probing. Losing that write to a stale
// reading is how an agent goes back to a primary that just died mid-run.
func clearSourceIf(agentName string, expected sourceSelection) bool {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	current, ok := selectedSources.by[agentName]
	if !ok || current != expected {
		return false
	}

	delete(selectedSources.by, agentName)

	return true
}

// selectedSource returns the fallback selection preflight (or a served
// mid-run swap) chose for an agent, if any. internal/pipeline has no need for
// it; the agent step itself applies it.
func selectedSource(agentName string) (sourceSelection, bool) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	selection, ok := selectedSources.by[agentName]

	return selection, ok
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

// probeAgentServers checks the MCP servers one agent's tool grant names.
func probeAgentServers(ctx context.Context, cfg *config.Config, ri config.ResolvedInvocation, settings *config.Preflight, seen map[string]bool) []config.Problem {
	var problems []config.Problem

	for _, spec := range ri.ToolSpecs {
		if spec.MCP == "" || seen[spec.MCP] {
			continue
		}

		seen[spec.MCP] = true

		err := probeServerCached(ctx, cfg, spec, settings)
		if err != nil {
			problems = append(problems, config.Problem{
				Target:    fmt.Sprintf("mcp %q", spec.MCP),
				Detail:    err.Error(),
				Transient: isTransient(err),
			})
		}
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

// cliProbeKey is the probe cache key for a CLI target. Image is part of it
// because it changes what the probe ASKS: an image-less target is answered by
// a PATH lookup on this host, an image-bearing one by starting that image and
// checking credentials. A shared key would let either answer stand in for the
// other.
func cliProbeKey(ri config.ResolvedInvocation) string {
	return "cli|" + ri.CLI + "|" + ri.ModelName + "|" + ri.Image
}

func probeModelCached(ctx context.Context, ri config.ResolvedInvocation, settings *config.Preflight) error {
	// A CLI target is keyed on its own axis: "" is a perfectly ordinary
	// BaseURL for a CLI source, so a shared key space would let a CLI agent
	// and an endpoint-less HTTP one collide.
	//
	// APIKeyEnv is part of the key — an env var NAME, never its value —
	// because a probe exercises the credential too. A `fallback:` entry that
	// differs from its primary only in api_key_env is the documented key-
	// rotation shape (see nextViableFallback), and without this the fallback
	// read the PRIMARY's cached failure: a source that was serving runs
	// looked dead to reconsiderPin and to failOver alike.
	key := "model|" + ri.BaseURL + "|" + ri.ModelName + "|" + ri.APIKeyEnv
	if ri.CLI != "" {
		key = cliProbeKey(ri)
	}

	now := time.Now()

	found, cached := probeCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.Debug("preflight.cached", "target", key)

		return cached
	}

	err := probeModel(ctx, ri, settings.ProbeTimeout())
	probeCache.store(key, err, now)

	return err
}

// probeModel sends the smallest possible completion request and reports how it
// went. It goes through the same client construction a real step uses, so the
// endpoint, the model name, and the credential are all exercised — a probe
// that bypassed any of them would pass for a run that could not start.
func probeModel(ctx context.Context, ri config.ResolvedInvocation, timeout time.Duration) error {
	if ri.CLI != "" {
		return probeCLI(ctx, ri, timeout)
	}

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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

	llm := newAgentLLM(ri, apiKey)

	for _, respErr := range llm.GenerateContent(probeCtx, req, false) {
		if respErr != nil {
			// A model that answered badly, or not at all, is a fact about
			// this minute — an outage, a rate limit, a cold start. The
			// credential problems that are NOT are caught above, before any
			// request is sent.
			return transient(describeProbeError(probeCtx, respErr, timeout))
		}

		slog.Debug("preflight.model_ok", "model", ri.ModelName, "elapsed", time.Since(started))

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

func probeServerCached(ctx context.Context, cfg *config.Config, spec config.ToolSpec, settings *config.Preflight) error {
	// MCPTool belongs in the key as much as MCPTools does, and leaving it out
	// was a hole in exactly the check this function performs. probeServer
	// asks two questions — does the server answer, and does it expose the
	// tools THIS grant names — and the second one has a different answer per
	// grant. Keyed without MCPTool, `{mcp: x, tool: a}` and `{mcp: x, tool:
	// b}` shared one entry, so whichever ran first answered for both: a grant
	// naming a tool the server does not expose passed preflight on the
	// strength of a different grant's tool existing. Silent, and only in a
	// process that had already probed the same server — which is every
	// `steps watch`.
	key := "mcp|" + spec.MCP + "|" + spec.MCPTool + "|" + strings.Join(spec.MCPTools, ",")
	now := time.Now()

	found, cached := probeCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.Debug("preflight.cached", "target", key)

		return cached
	}

	err := probeServer(ctx, cfg, spec, settings.ProbeTimeout())
	probeCache.store(key, err, now)

	return err
}

// probeServer starts an MCP server, confirms it comes up, and confirms the
// tools the pipeline grants actually exist on it. The second half matters as
// much as the first: a server that starts but no longer exposes
// `go_symbol_references` fails the step just as surely as one that never
// started, and just as late.
func probeServer(ctx context.Context, cfg *config.Config, spec config.ToolSpec, timeout time.Duration) error {
	srv, err := cfg.FindMCPServer(spec.MCP)
	if err != nil {
		return err //nolint:wrapcheck // FindMCPServer already names the server and lists the alternatives
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tools, err := stepsmcp.ListServerTools(probeCtx, *srv)
	if err != nil {
		if probeCtx.Err() != nil {
			return transient(fmt.Errorf("did not start within %s", timeout))
		}

		// Transient by default: a server that did not come up is usually a
		// fact about this minute. transient() itself makes the exception for
		// an oauth token no poll can renew, which is the case that has to
		// stop a watcher rather than be retried by it.
		return transient(fmt.Errorf("could not start: %w", err))
	}

	_, err = selectMCPTools(spec, tools)
	if err != nil {
		// selectMCPTools already names the missing tool and lists what the
		// server does offer.
		return err
	}

	return nil
}
