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

// lookup returns a cached result when one is still within ttl. The entry
// carries WHEN the answer was established, which decisions that destroy state
// need — see probeModelCached.
func (c *resultCache) lookup(key string, ttl time.Duration, now time.Time) (cacheEntry, bool) {
	if ttl <= 0 {
		return cacheEntry{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) > ttl {
		return cacheEntry{}, false
	}

	return entry, true
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

	selectedSources.by = map[pinScope]pinRecord{}
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
			slog.Warn("agent.fallback_unavailable",
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
		slog.Warn("agent.failover",
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
// by a step failing. Both facts feed one pure decision (decidePreflightPin).
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
	primaryProbe := probeFact{healthy: primaryErr == nil, fresh: primaryAt.After(record.since)}

	candidate, err := primary.WithSource(selection.source, agent)
	if err != nil {
		// Unusable rather than unwell, and it has to say so: keeping this pin
		// fails every step of the run on the same resolution error, but
		// reporting it through the health path below would blame a probe that
		// was never sent and name the model it left as "".
		if clearSourceIf(scope, selection) == clearedPin {
			slog.Warn("agent.failover.pin_lost",
				"pipeline", scope.pipeline,
				"agent", agent.Name,
				"error", err,
				"reason", "the pinned fallback no longer resolves; re-deciding from the primary")
		}

		return false
	}

	// Probed only where its answer can still change the verdict:
	// decidePreflightPin short-circuits on a primary that is FRESHLY healthy,
	// and this is a real request against a provider the agent is about to
	// stop using — one that can spend the whole ProbeTimeout if the fallback
	// is the thing hanging. A merely cached positive no longer short-circuits
	// it, which is the point: that answer cannot end a pin on its own, so the
	// other fact is worth having.
	pinnedProbe := probeFact{}
	if !primaryProbe.recovered() {
		at, err := probeModelCached(ctx, candidate, settings)
		pinnedProbe = probeFact{healthy: err == nil, fresh: at.After(record.since)}
	}

	// A cancelled run learned nothing about either source: both probes fail
	// for a reason about the operator, not the endpoint. decideCascade calls
	// that sourceUnproven and leaves the pin alone; so does this. It reports
	// false rather than true so the caller still records the failure.
	if ctx.Err() != nil {
		return false
	}

	if decidePreflightPin(pinned, primaryProbe, pinnedProbe) != dropPin {
		// Both halves have to hold to report a keep. renewPinIf fails when a
		// concurrent cascade or expiry has already dropped this pin, and
		// claiming to keep one that is gone left the caller neither failing
		// over nor recording a failure — a green preflight with a dead primary
		// and no pin at all. pinnedProbe.healthy is the other half: see the
		// doc comment on what this bool is allowed to mean.
		return renewPinIf(scope, selection) && pinnedProbe.healthy
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
	primaryProbe probeFact,
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
	if primaryProbe.recovered() {
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

// pinRecord is a selection plus the two clocks that bound it. They are
// separate fields because they answer different questions and move at
// different times, and collapsing them into one was a defect in both
// directions.
//
// The selection is the IDENTITY, and it is what the compare-and-swap guards
// compare. Nothing else may join it: a timestamp inside the compared value
// makes a guard fail for a pin whose source and index never changed, which
// turned every concurrent renewal into a lost release.
type pinRecord struct {
	selection sourceSelection
	// since is when this agent LEFT its primary, and it never moves while the
	// pin stands. It is the bar a probe has to clear to be evidence about the
	// outage that caused this pin: an answer older than the pin was given
	// before the reason for it existed, whoever asked and in whichever pass.
	since time.Time
	// decided is when something last re-decided this pin from probes, which
	// is what pinLifetime counts down from. Separate from since precisely so
	// renewing the lifetime does not move the freshness bar — moving it meant
	// a renewal could make every existing answer stale, and a pin that can
	// never see a fresh answer can never be released at all.
	decided time.Time
}

// pinScope is WHOSE pin: an agent, in a pipeline.
//
// The pipeline half is the correction for a collision that was always latent
// and became live traffic once a pin gained an exit condition. One process
// serves several pipelines (`steps web app.yml infra.yml`, `steps watch` over
// a shared state file), two of them may declare an agent called `reviewer`
// with entirely different source: blocks, and keyed by name alone the first
// one's outage resolved the second onto an endpoint it never declared —
// probing it, and running steps against it.
//
// It is the same unscoped-global shape internal/store answers with a
// pipeline_id on every row, one package over, and it takes the same posture:
// the scope is part of the key, so a caller cannot forget it.
type pinScope struct {
	pipeline string
	agent    string
}

// agentPinScope names an agent's pin within the pipeline that declared it.
// config.Path is stamped by the loader, so every real run has one; a Config
// built in a test rather than loaded shares the empty scope, as everything
// did before this existed.
func agentPinScope(cfg *config.Config, agentName string) pinScope {
	return pinScope{pipeline: cfg.Path, agent: agentName}
}

// pinLifetime is how long an agent stays on a fallback without anything
// re-affirming that its primary is still unwell.
//
// It exists because the pin's other exit condition — reconsiderPin, at the
// pre-run probe — lives inside a check three separate switches turn off
// (`preflight: false` on the agent, `defaults.preflight.disabled:`,
// `--no-preflight`), while the mid-run cascade that INSTALLS a pin is gated
// by none of them. A release that is a feature of a check you can disable is
// not a release: in exactly the configuration docs/agents.md recommends for a
// cold local model — the one that also names a paid hosted fallback — a
// ninety-second blip pinned a `steps watch` for the life of the process.
//
// So the expiry rides on the pin itself, evaluated wherever one is read, and
// preflight stays what it always was: the cheaper, better-informed way to
// reach the same decision sooner, using a real probe instead of a clock.
//
// Fifteen minutes is the trade between the two costs. Expiring re-decides
// from the primary, which with no probe available costs one attempts: cycle
// against a source that may still be dead — slow, but bounded and paid at
// most once per lifetime per agent. NOT expiring costs every step of every
// run on a fallback that may be a paid provider, indefinitely. It is
// comfortably longer than defaults.preflight.cache: (5m), so a pin still
// outlives the probe answer that created it when preflight is on at all.
//
// The value lives in internal/config so validatePreflight can refuse a
// defaults.preflight.cache: longer than it — see config.PinLifetime.
const pinLifetime = config.PinLifetime

// selectedSources records which fallback an agent is running on, across runs.
// Process-scoped like probeCache and for the same reason: a `steps watch`
// that failed over should stay failed over rather than re-failing-over on
// every poll.
//
// Keyed by pinScope, so a pipeline's pin is its own. It has two exit
// conditions and needs both: reconsiderPin, which is the informed one and is
// reachable only when preflight runs, and pinLifetime, which is the one that
// still works when it does not.
//
//nolint:gochecknoglobals // process-lifetime selection, deliberately shared across runs
var selectedSources = struct {
	mu sync.Mutex
	by map[pinScope]pinRecord
}{by: map[pinScope]pinRecord{}}

// selectSource pins an agent to the fallback entry at index for the rest of
// the process.
//
// Callers pin a source that has PROVED itself — answered a preflight probe,
// or served a conversation through to its own conclusion — never one merely
// chosen. Pinning on selection alone strands the process on a source that
// never answered until reconsiderPin's next probe notices, which is a whole
// run's worth of steps aimed at something known not to work.
func selectSource(scope pinScope, source config.AgentSource, index int) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	now := time.Now()
	record := pinRecord{
		selection: sourceSelection{source: source, index: index},
		since:     now,
		decided:   now,
	}

	// since never moves while a pin stands: it dates the PRIMARY's outage,
	// and nothing that happens among the fallbacks speaks to that.
	//
	// decided is the lifetime's clock, and it turns on whether this is the
	// same pin or a different one. Re-pinning the SAME source says nothing
	// new — every step that runs on a pinned fallback and serves re-pins it
	// (decideCascade returns pinThisSource for any source that carried a
	// conversation), so restarting the lifetime there would mean a busy
	// pipeline never re-decides at all. A cascade that MOVED is a fresh
	// decision backed by fresh evidence: the source it just installed carried
	// a conversation seconds ago. Handing it the old source's spent clock
	// dropped it on the very next read, and the run then paid a cycle at the
	// dead primary and another at the dead fallback to arrive back where it
	// already was — twice inside a window docs/agents.md promises costs one.
	if existing, ok := selectedSources.by[scope]; ok {
		record.since = existing.since

		if existing.selection == record.selection {
			record.decided = existing.decided
		}
	}

	selectedSources.by[scope] = record
}

// renewPinIf restarts a pin's LIFETIME, while it still holds the selection the
// caller evaluated, and reports whether it did.
//
// Compare-and-set for the same reason clearSourceIf is compare-and-delete, and
// the report matters for the same reason: a guard that quietly does nothing is
// a caller believing it wrote. reconsiderPin returns keepPin on the strength
// of this, and a pin another goroutine has since dropped is not one to keep.
//
// It touches `decided` and never `since`. Moving the freshness bar forward on
// a renewal would make every answer already in the probe cache stale, and a
// pin that cannot see a fresh answer cannot be released by one — the renewal
// would quietly become the thing that makes the pin permanent.
//
// One caller, deliberately: the pre-run probe, on the branch where it decides
// to KEEP a pin. It is what divides the pin's two exits — while preflight runs
// it re-decides every run from probes and the clock never runs out; where
// preflight is switched off nothing renews it and the clock is the only exit
// there is.
func renewPinIf(scope pinScope, expected sourceSelection) bool {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	current, ok := selectedSources.by[scope]
	if !ok || current.selection != expected {
		return false
	}

	current.decided = time.Now()
	selectedSources.by[scope] = current

	return true
}

// clearSource drops an agent's pin, so the next run resolves from its primary
// again. Called when a cascade tried every source and none served: continuing
// to prefer the last one tried would be preferring a source that just failed.
// reconsiderPin drops a pin for its own reasons, through clearSourceIf.
func clearSource(scope pinScope) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	delete(selectedSources.by, scope)
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
//
// It compares the SELECTION and not the whole record, which is the reason
// pinRecord keeps its clocks outside it: with a timestamp in the compared
// value, a concurrent pass that merely renewed the lifetime made this guard
// fail for a pin whose source and index had not moved at all — turning every
// such race into a silently lost release rather than a detected one.
func clearSourceIf(scope pinScope, expected sourceSelection) clearOutcome {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	current, ok := selectedSources.by[scope]
	if !ok {
		return pinAlreadyGone
	}

	if current.selection != expected {
		return pinReplaced
	}

	delete(selectedSources.by, scope)

	return clearedPin
}

// clearOutcome distinguishes the two ways a compare-and-delete can decline,
// because they call for opposite handling and one bool conflated them.
//
// pinAlreadyGone means there is no pin: the caller should fail over, since
// nothing is holding the agent anywhere. pinReplaced means a live pin exists
// that a concurrent writer installed on newer evidence than this pass read —
// failing over there walks the fallback list from index 0 and demotes the
// agent off the entry another job's cascade just proved, which is precisely
// what the guard exists to prevent.
type clearOutcome int

const (
	clearedPin clearOutcome = iota
	pinAlreadyGone
	pinReplaced
)

// selectedSource returns the fallback selection preflight (or a served
// mid-run swap) chose for an agent, if any. internal/pipeline has no need for
// it; the agent step itself applies it.
//
// Reading is where a pin expires, because reading is the one thing every path
// to a pin has in common — the pre-run probe, a step resolving its source,
// the cascade. Hanging the lifetime off any single writer would leave it
// exactly as skippable as reconsiderPin already is.
func selectedSource(scope pinScope) (sourceSelection, bool) {
	return selectedSourceAt(scope, time.Now())
}

// runBoundaryKey carries the instant a run began.
type runBoundaryKey struct{}

// WithRunBoundary stamps a context with the moment its run started, which is
// the instant every pin read inside that run is judged against.
//
// The lifetime still rides on the pin and is still applied wherever one is
// read — hanging it off a writer would leave it exactly as skippable as
// reconsiderPin. What this fixes is WHICH clock the read consults. Reading at
// wall-clock meant a job of six agent steps could cross the lifetime between
// steps 4 and 5: the pin vanished mid-job and the remaining steps resolved to
// a primary that was still down, each paying a full attempts: cycle before
// cascading and re-pinning. Nothing renews mid-job, because renewPinIf is
// reachable only from Preflight — once per job, and switched off entirely by
// --no-preflight, which is the configuration the lifetime exists to serve.
//
// Judging against the run's own start makes the release land at a boundary
// where nothing is half-done, which is what docs/agents.md has always
// promised: a run decides before it starts, and one job sees a settled source
// throughout. internal/pipeline stamps this once per job run, beside the
// OpenRouter session, so concurrent jobs each carry their own.
func WithRunBoundary(ctx context.Context) context.Context {
	return context.WithValue(ctx, runBoundaryKey{}, time.Now())
}

// runInstant is when the current run began, or now for a caller that never
// declared a run — every non-pipeline caller, and every test.
func runInstant(ctx context.Context) time.Time {
	at, ok := ctx.Value(runBoundaryKey{}).(time.Time)
	if !ok {
		return time.Now()
	}

	return at
}

// selectedSourceAt is selectedSource judged against a caller-chosen instant.
func selectedSourceAt(scope pinScope, now time.Time) (sourceSelection, bool) {
	selectedSources.mu.Lock()

	record, ok := selectedSources.by[scope]

	expired := ok && now.Sub(record.decided) >= pinLifetime
	if expired {
		delete(selectedSources.by, scope)
	}

	selectedSources.mu.Unlock()

	if expired {
		// As loud as arriving and as loud as returning: an operator watching
		// for a change in model quality needs every edge, and this one moves
		// an agent back to its primary with no probe behind it.
		slog.Warn("agent.failover.pin_expired",
			"pipeline", scope.pipeline,
			"agent", scope.agent,
			"model", record.selection.source.Model,
			"lifetime", pinLifetime,
			"reason", "the pin outlived its evidence; re-deciding from the primary")

		return sourceSelection{}, false
	}

	return record.selection, ok
}

// pinnedSource is selectedSource without the lifetime: whatever is recorded,
// however old, with the moment the agent left its primary. Only reconsiderPin
// wants this — see the note there.
func pinnedSource(scope pinScope) (pinRecord, bool) {
	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	record, ok := selectedSources.by[scope]

	return record, ok
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
		// Deduped by GRANT, not by server. Deduping by name meant the
		// second grant on a server never reached the cache at all, so
		// whether a bad tool name was caught came down to which agent was
		// listed first. cfg is fixed for one Preflight, so the grant is the
		// whole identity here — the definition belongs in the cache key,
		// which outlives this call.
		if spec.MCP == "" {
			continue
		}

		key := spec.MCP + "|" + spec.MCPTool + "|" + strings.Join(spec.MCPTools, ",")
		if seen[key] {
			continue
		}

		seen[key] = true

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

// probeModelCached probes a model, or answers from the cache, and reports
// WHEN the answer it returns was established.
//
// The timestamp is not bookkeeping: a decision that DESTROYS state has to
// insist on a fact from now. Collapsing it let a cached positive taken before
// an outage release a pin the mid-run cascade earned during one — the step
// then ran at the still-broken primary, cascaded, re-pinned, and did it again
// on the next poll, logging "the primary model answered preflight again"
// about a question nobody had asked in five minutes.
//
// A time rather than a did-I-send-it bool, because "just now" is a property
// of the ANSWER and not of the caller. Several agents legitimately share one
// target, so within a single preflight pass the first of them sends the
// request and the rest read it back out of the cache microseconds later — all
// of them holding an answer from this pass. Reporting that as stale stranded
// every agent but the first on its fallback permanently, since the keep also
// renews pinLifetime.
func probeModelCached(ctx context.Context, ri config.ResolvedInvocation, settings *config.Preflight) (time.Time, error) {
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

	entry, found := probeCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.Debug("preflight.cached", "target", key)

		return entry.at, entry.err
	}

	err := probeModel(ctx, ri, settings.ProbeTimeout())

	// A probe the CALLER abandoned learned nothing about the target. The
	// probe's own deadline firing IS a fact about the target and is cached;
	// ctx being done means Ctrl-C, a job deadline, or a shutdown, and
	// describeProbeError renders both identically, so without this an
	// aborted build left a healthy model looking dead for the whole cache
	// window.
	if ctx.Err() != nil {
		return now, err
	}

	probeCache.store(key, err, now)

	return now, err
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
	// The DEFINITION, not just the name. The cache is process-wide and
	// pipeline-blind, and a name is not an identity: under
	// `steps web app.yml infra.yml` two pipelines may each declare a server
	// called `test` pointing at different things, and one verdict served
	// both — a healthy server vouching for a broken neighbour, and a broken
	// one condemning a healthy neighbour. Naming the definition is stronger
	// than naming the pipeline: it also separates two revisions of one
	// pipeline. internal/resource/preflight.go keys its sibling cache the
	// same way and was immune to both.
	srv, err := cfg.FindMCPServer(spec.MCP)
	if err != nil {
		// A name no server answers to is a config error, identical on every
		// call, and there is no definition to key it under.
		return err //nolint:wrapcheck // FindMCPServer already names the server and lists the alternatives
	}

	key := strings.Join(append(
		[]string{"mcp", srv.Name, srv.Endpoint, srv.Command, srv.Cwd},
		append(append([]string{}, srv.Args...), spec.MCPTool, strings.Join(spec.MCPTools, ","))...,
	), "|")
	now := time.Now()

	entry, found := probeCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.Debug("preflight.cached", "target", key)

		return entry.err
	}

	err = probeServer(ctx, *srv, spec, settings.ProbeTimeout())

	// A probe the CALLER abandoned learned nothing about the target. The
	// probe's own deadline firing IS a fact about the target and is cached;
	// ctx being done means Ctrl-C, a job deadline, or a shutdown, and
	// describeProbeError renders both identically, so without this an
	// aborted build left a healthy server looking dead for the whole cache
	// window.
	if ctx.Err() != nil {
		return err
	}

	probeCache.store(key, err, now)

	return err
}

// probeServer starts an MCP server, confirms it comes up, and confirms the
// tools the pipeline grants actually exist on it. The second half matters as
// much as the first: a server that starts but no longer exposes
// `go_symbol_references` fails the step just as surely as one that never
// started, and just as late.
func probeServer(ctx context.Context, srv config.MCPServer, spec config.ToolSpec, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tools, err := stepsmcp.ListServerTools(probeCtx, srv)
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
