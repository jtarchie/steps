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
// 500 arrived at 08:48. Failing over mid-run is a different feature.

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

// ResetProbeCache clears everything preflight has verified. Tests use it to
// stay independent of each other.
func ResetProbeCache() {
	probeCache.mu.Lock()
	probeCache.entries = map[string]cacheEntry{}
	probeCache.mu.Unlock()

	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	selectedSources.by = map[string]config.AgentSource{}
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

		ri, err := cfg.ResolveAgentInvocation(config.Step{Agent: name})
		if err != nil {
			continue // an unresolvable agent is already a load error
		}

		// Every agent gets its own decision, even when several share a model.
		// Deduping by (endpoint, model) and skipping the later agents left
		// them with no failover recorded while preflight reported zero
		// problems — the run then died mid-plan against the dead primary,
		// which is exactly the failure preflight exists to move earlier.
		// probeModelCached already collapses the network cost to one request.
		probeErr := probeModelCached(ctx, ri, settings)
		if probeErr == nil {
			healthyEndpoints[ri.BaseURL] = true
		} else if !failOver(ctx, agent, ri, settings) {
			failures = append(failures, modelFailure{name: name, ri: ri, err: probeErr})
		}

		problems = append(problems, probeAgentServers(ctx, cfg, ri, settings, seenServer)...)
	}

	return append(problems, renderModelFailures(failures, healthyEndpoints)...)
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
		candidate, err := primary.WithSource(agent.Fallback[i].Source, agent.CompactAfterTokens)
		if err != nil {
			continue // an unresolvable fallback is already a load error
		}

		probeErr := probeModelCached(ctx, candidate, settings)
		if probeErr != nil {
			slog.Warn("agent.fallback_unavailable",
				"agent", agent.Name, "fallback", i, "model", candidate.ModelName, "error", probeErr)

			continue
		}

		selectSource(agent.Name, agent.Fallback[i].Source)

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

// selectedSources records which fallback an agent is running on, for the life
// of the process. Process-scoped like probeCache and for the same reason: a
// `steps watch` that failed over should stay failed over rather than
// re-probing a known-dead primary on every poll.
//
//nolint:gochecknoglobals // process-lifetime selection, deliberately shared across runs
var selectedSources = struct {
	mu sync.Mutex
	by map[string]config.AgentSource
}{by: map[string]config.AgentSource{}}

func selectSource(agentName string, source config.AgentSource) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	selectedSources.by[agentName] = source
}

// SelectedSource returns the fallback source preflight chose for an agent, if
// any. internal/pipeline has no need for it; the agent step itself applies it.
func selectedSource(agentName string) (config.AgentSource, bool) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	source, ok := selectedSources.by[agentName]

	return source, ok
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
			Target: fmt.Sprintf("agent %q", failure.name),
			Detail: detail,
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
				Target: fmt.Sprintf("mcp %q", spec.MCP),
				Detail: err.Error(),
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

func probeModelCached(ctx context.Context, ri config.ResolvedInvocation, settings *config.Preflight) error {
	// A CLI target is keyed on its own axis: "" is a perfectly ordinary
	// BaseURL for a CLI source, so a shared key space would let a CLI agent
	// and an endpoint-less HTTP one collide.
	key := "model|" + ri.BaseURL + "|" + ri.ModelName
	if ri.CLI != "" {
		key = "cli|" + ri.CLI + "|" + ri.ModelName
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
		return probeCLI(ri)
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
			return describeProbeError(probeCtx, respErr, timeout)
		}

		slog.Debug("preflight.model_ok", "model", ri.ModelName, "elapsed", time.Since(started))

		return nil
	}

	return errors.New("the model returned no response at all")
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
	key := "mcp|" + spec.MCP + "|" + strings.Join(spec.MCPTools, ",")
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
			return fmt.Errorf("did not start within %s", timeout)
		}

		return fmt.Errorf("could not start: %w", err)
	}

	_, err = selectMCPTools(spec, tools)
	if err != nil {
		// selectMCPTools already names the missing tool and lists what the
		// server does offer.
		return err
	}

	return nil
}
