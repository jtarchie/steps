package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// expireProbes clears what preflight has verified WITHOUT touching the pins.
//
// ResetProbeCache deliberately clears both, which is right for a test wanting
// a clean process — and useless here: wiping the pin is the very thing under
// test, so a scenario built on it would pass whether the release worked or
// not (verified: it did).
func expireProbes() {
	probeCache.mu.Lock()
	defer probeCache.mu.Unlock()

	probeCache.entries = map[string]cacheEntry{}
}

// testPin is the scope of a pin belonging to a Config that was built rather
// than loaded: config.Name is empty, so every such Config shares one scope —
// which is what every pin shared before pinScope existed. Tests that are
// about the SCOPE build two real pipelines instead (see the root package's
// mid-run failover e2e).
func testPin(agentName string) pinScope {
	return pinScope{pipeline: "", agent: agentName}
}

// togglableProbeEndpoint serves a probe that can be taken down and brought
// back, which is the whole scenario: a pin only has an exit condition if
// something can recover.
func togglableProbeEndpoint(t *testing.T) (url string, up *atomic.Bool) {
	t.Helper()

	live := &atomic.Bool{}
	live.Store(true)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !live.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"probe","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)

	return server.URL + "/v1/", live
}

// pinConfig is one agent with one hosted fallback, both pointed at endpoints
// the test can take down independently.
//
// attempts: 1 because none of these tests is about retry behavior, and the
// default 3 spends the whole suite in backoff waiting on endpoints it has
// deliberately taken down.
func pinConfig(t *testing.T, name, primaryURL, fallbackURL string) *config.Config {
	t.Helper()

	return pinConfigWithFallbacks(t, name, primaryURL, pinSource("openai/backup", fallbackURL))
}

// pinSource is one hosted source. APIKeyEnv is an env var NAME, not a
// credential, which is the whole reason this is a helper rather than a literal
// repeated at four call sites gosec flags individually.
//
//nolint:gosec // an env var NAME, not a credential
func pinSource(model, endpoint string) config.AgentSource {
	return config.AgentSource{Model: model, Endpoint: endpoint, APIKeyEnv: "OPENAI_API_KEY"}
}

func pinConfigWithFallbacks(t *testing.T, name, primaryURL string, sources ...config.AgentSource) *config.Config {
	t.Helper()

	// Both halves of the process-global state, restored both ways: these
	// tests leave probe entries keyed by httptest URLs that are closed
	// moments later, and a pin that outlives its endpoint is exactly the
	// cross-test contamination isolatePreflightPins exists for upstream.
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	attempts := 1

	fallbacks := make([]config.AgentFallback, 0, len(sources))
	for _, source := range sources {
		fallbacks = append(fallbacks, config.AgentFallback{Source: source})
	}

	return &config.Config{
		Agents: []config.Agent{{
			Name:     name,
			Attempts: &attempts,
			Source:   pinSource("openai/primary", primaryURL),
			Fallback: fallbacks,
		}},
	}
}

// TestPreflightReturnsToARecoveredPrimary is the defect this closes. A pin is
// process-lifetime and had no way back, so a ninety-second outage could move
// a `steps web` agent to a possibly-worse model for days — silently, since
// agent.failover fires once at the swap and nothing afterwards says the agent
// is still on the fallback. Preflight was already re-probing the primary
// every run and discarding the answer.
func TestPreflightReturnsToARecoveredPrimary(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)
	cfg := pinConfig(t, "returner", primaryURL, fallbackURL)
	logs := captureLogs(t)

	// The outage: the primary is down, so preflight fails over and pins.
	primaryUp.Store(false)

	if problems := Preflight(t.Context(), cfg, []string{"returner"}, &config.Preflight{}); len(problems) != 0 {
		t.Fatalf("problems = %+v, want none — the fallback should have absorbed the outage", problems)
	}

	selection, pinned := selectedSource(testPin("returner"))
	if !pinned || selection.source.Model != "openai/backup" {
		t.Fatalf("selection = %+v (pinned=%v), want the fallback pinned", selection, pinned)
	}

	// The recovery. A fresh run means a fresh probe: the cache TTL is what
	// bounds how stale this answer may be, and expiring it is what a later
	// poll does. The PIN survives, which is the point.
	primaryUp.Store(true)
	expireProbes()

	if problems := Preflight(t.Context(), cfg, []string{"returner"}, &config.Preflight{}); len(problems) != 0 {
		t.Fatalf("problems = %+v, want none — the primary is answering again", problems)
	}

	if _, stillPinned := selectedSource(testPin("returner")); stillPinned {
		t.Error("the agent is still pinned to the fallback after its primary recovered")
	}

	// The event name is the contract docs/agents.md publishes, and the only
	// channel this edge has — nothing about a preflight-boundary release
	// reaches a step's output line or the recorded result.
	if !strings.Contains(logs.String(), "agent.failover.returned") {
		t.Errorf("the return was silent:\n%s", logs.String())
	}

	// And it says WHOSE. These lines fire with no run to place them, so in a
	// process serving several pipelines that each declare the same agent name
	// a line naming only the agent is one an operator cannot act on.
	if !strings.Contains(logs.String(), "pipeline=") {
		t.Errorf("the release does not name the pipeline it belongs to:\n%s", logs.String())
	}
}

// TestPreflightKeepsAServingFallback is the property the fix must not break.
// Returning eagerly is how a flapping primary oscillates an agent between
// models; while the primary is still down and the fallback is still
// answering, the pin is exactly what should happen.
func TestPreflightKeepsAServingFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)
	cfg := pinConfig(t, "stayer", primaryURL, fallbackURL)

	primaryUp.Store(false)
	Preflight(t.Context(), cfg, []string{"stayer"}, &config.Preflight{})

	if _, pinned := selectedSource(testPin("stayer")); !pinned {
		t.Fatal("no fallback was pinned for an agent whose primary is down")
	}

	// A second run, primary still down.
	expireProbes()
	Preflight(t.Context(), cfg, []string{"stayer"}, &config.Preflight{})

	selection, pinned := selectedSource(testPin("stayer"))
	if !pinned {
		t.Fatal("the pin was dropped while the primary was still down and the fallback still serving")
	}

	if selection.source.Model != "openai/backup" {
		t.Errorf("selection = %q, want the fallback still preferred", selection.source.Model)
	}
}

// TestPreflightRedecidesWhenThePinnedSourceDies covers the blind spot that
// fixing the first one would otherwise have widened. Preflight probes the
// primary; a pinned fallback that dies was therefore discovered by a step
// failing rather than by the probe. Probing both is what makes "primary
// healthy" and "pinned source dead" independent reasons to re-decide.
func TestPreflightRedecidesWhenThePinnedSourceDies(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, fallbackUp := togglableProbeEndpoint(t)
	cfg := pinConfig(t, "stranded", primaryURL, fallbackURL)
	logs := captureLogs(t)

	primaryUp.Store(false)
	Preflight(t.Context(), cfg, []string{"stranded"}, &config.Preflight{})

	if _, pinned := selectedSource(testPin("stranded")); !pinned {
		t.Fatal("no fallback was pinned for an agent whose primary is down")
	}

	// Now both are down. The pin points at something that cannot serve, and
	// keeping it would mean the next step discovers that by failing.
	fallbackUp.Store(false)
	expireProbes()

	problems := Preflight(t.Context(), cfg, []string{"stranded"}, &config.Preflight{})
	if len(problems) == 0 {
		t.Error("preflight reported no problems with every source down")
	}

	if _, pinned := selectedSource(testPin("stranded")); pinned {
		t.Error("the agent is still pinned to a fallback that stopped answering")
	}

	if !strings.Contains(logs.String(), "agent.failover.pin_lost") {
		t.Errorf("losing the pinned source was silent:\n%s", logs.String())
	}
}

// TestPreflightDoesNotDemoteAPinnedFallback is the property
// TestPreflightKeepsAServingFallback claims and cannot check: with a single
// fallback entry, "the pin survived" and "failOver re-selected index 0" are
// the same observable, so that test passes even when the pin is dropped.
//
// With the pin at index 1 they separate. failOver walks agent.Fallback from
// index 0 unconditionally and never reads the pin, so anything that lets it
// run for a still-serving pinned agent silently trades a source that carried
// a whole conversation for one that answered a single token — and re-emits
// agent.failover every cache window for a swap that already happened.
func TestPreflightDoesNotDemoteAPinnedFallback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	firstURL, _ := togglableProbeEndpoint(t)
	secondURL, _ := togglableProbeEndpoint(t)

	cfg := pinConfigWithFallbacks(t, "demoted", primaryURL,
		pinSource("openai/first", firstURL),
		pinSource("openai/second", secondURL),
	)

	// As a mid-run cascade that walked past a then-unwell fallback[0] would
	// leave it: pinned to the entry that actually served.
	primaryUp.Store(false)
	selectSource(testPin("demoted"), cfg.Agents[0].Fallback[1].Source, 1)

	Preflight(t.Context(), cfg, []string{"demoted"}, &config.Preflight{})

	selection, pinned := selectedSource(testPin("demoted"))
	if !pinned {
		t.Fatal("the pin was dropped while the primary was down and the pinned fallback still serving")
	}

	if selection.index != 1 || selection.source.Model != "openai/second" {
		t.Errorf("selection = %q at index %d, want openai/second at index 1",
			selection.source.Model, selection.index)
	}
}

// agePin backdates a pin's clock, which is the only way to observe a lifetime
// measured in minutes from a test measured in milliseconds.
//
// It reaches into selectedSources directly rather than through an exported
// knob: the lifetime is not a tunable, and adding a seam to production code
// so a test can move a clock would be inventing one.
func agePin(t *testing.T, scope pinScope, by time.Duration) {
	t.Helper()

	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	record, ok := selectedSources.by[scope]
	if !ok {
		t.Fatalf("no pin to age for %+v", scope)
	}

	// Both clocks, because a pin that is older is older in both senses: it was
	// installed further back, and whatever last re-decided it did so further
	// back. Ageing only one would test a state nothing produces.
	record.since = record.since.Add(-by)
	record.decided = record.decided.Add(-by)
	selectedSources.by[scope] = record
}

// offlinePinConfig is an agent that never gets a pre-run probe: preflight:
// false is the documented recommendation for a cold local model, and a cold
// local model is precisely the setup that also names a paid hosted fallback.
//
// The endpoints are unreachable on purpose. Nothing in this scenario sends a
// request — that is the scenario.
func offlinePinConfig(t *testing.T, name string) *config.Config {
	t.Helper()

	cfg := pinConfig(t, name, "http://127.0.0.1:1/v1/", "http://127.0.0.1:2/v1/")

	off := false
	cfg.Agents[0].Preflight = &off

	return cfg
}

// TestAPinSurvivesWithoutPreflightUntilItsLifetimeIsUp is the sharp gap #75
// left behind: the ENTRY to a pin and its EXIT lived in different places.
// pinServedSource installs one from the mid-run cascade unconditionally, while
// reconsiderPin releases one only from inside Preflight — which `preflight:
// false`, `defaults.preflight.disabled:` and `--no-preflight` each skip
// entirely. A release that is a feature of a check you can turn off is not a
// release, and a ninety-second blip pinned a `steps web` for the life of
// the process in exactly the configuration most likely to hit it.
func TestAPinSurvivesWithoutPreflightUntilItsLifetimeIsUp(t *testing.T) {
	cfg := offlinePinConfig(t, "offline")
	scope := agentPinScope(cfg, "offline")
	step := config.Step{Agent: "offline"} //nolint:exhaustruct // only the agent name is read
	logs := captureLogs(t)

	// What the mid-run cascade does when a primary dies partway through a
	// conversation. No preflight ran, and none will.
	pinServedSource(scope, &cfg.Agents[0], 0)

	_, effective, _, index, err := resolveWithFailover(context.Background(), cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover: %v", err)
	}

	if effective.ModelName != "backup" || index != 0 {
		t.Fatalf("the step resolved to %q at index %d, want the pinned fallback — a fresh pin must hold", effective.ModelName, index)
	}

	agePin(t, scope, pinLifetime)

	_, effective, _, index, err = resolveWithFailover(context.Background(), cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover after the lifetime: %v", err)
	}

	if effective.ModelName != "primary" || index != -1 {
		t.Errorf("the step resolved to %q at index %d, want the primary — the pin outlived its evidence with nothing able to end it",
			effective.ModelName, index)
	}

	if !strings.Contains(logs.String(), "agent.failover.pin_expired") {
		t.Errorf("the pin was dropped silently:\n%s", logs.String())
	}
}

// TestAServingFallbackDoesNotRefreshThePinsClock is what makes the lifetime
// mean anything on a pipeline that is actually working.
//
// Every step that runs on a pinned fallback and reaches a conclusion re-pins
// it — decideCascade returns pinThisSource for any source that carried a
// conversation. If that reset the clock, a busy `steps web` would refresh
// the pin faster than it could ever expire and the exit condition would be
// unreachable in precisely the deployment it was written for. Using a
// fallback says nothing whatever about the primary it replaced.
func TestAServingFallbackDoesNotRefreshThePinsClock(t *testing.T) {
	cfg := offlinePinConfig(t, "busy")
	scope := agentPinScope(cfg, "busy")

	pinServedSource(scope, &cfg.Agents[0], 0)
	agePin(t, scope, pinLifetime)

	// A step runs on the pinned fallback and serves, exactly as every step of
	// every poll would.
	pinServedSource(scope, &cfg.Agents[0], 0)

	if _, pinned := selectedSource(scope); pinned {
		t.Error("a step serving on the fallback renewed the pin's lifetime — a busy pipeline would never re-decide")
	}
}

// TestPreflightKeepsAPinAgainstACachedRecovery is the second gap: a release
// could fire on an answer nobody had asked for.
//
// The sequence, all real here rather than argued: the primary answers and the
// answer caches; the primary then dies and a mid-run cascade pins the
// fallback that carried the step; the next run's probe is a pure cache hit
// returning the pre-outage success. Releasing there sent the step back to the
// still-broken primary — which cascaded, re-pinned, and did it again on the
// next poll — while logging that the primary had answered preflight again.
func TestPreflightKeepsAPinAgainstACachedRecovery(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)
	cfg := pinConfig(t, "cached", primaryURL, fallbackURL)
	scope := agentPinScope(cfg, "cached")
	logs := captureLogs(t)

	// A healthy run: the primary answers, and that answer is now cached for
	// defaults.preflight.cache:.
	if problems := Preflight(t.Context(), cfg, []string{"cached"}, &config.Preflight{}); len(problems) != 0 {
		t.Fatalf("problems = %+v, want none from a healthy primary", problems)
	}

	// The outage, and the cascade's response to it. Note what is NOT done
	// here: the probe cache is not expired, because a real outage does not
	// expire it either.
	primaryUp.Store(false)
	pinServedSource(scope, &cfg.Agents[0], 0)

	Preflight(t.Context(), cfg, []string{"cached"}, &config.Preflight{})

	selection, pinned := selectedSource(scope)
	if !pinned {
		t.Fatal("the pin was dropped on a cached positive taken before the outage that earned it")
	}

	if selection.source.Model != "openai/backup" {
		t.Errorf("selection = %q, want the fallback still pinned", selection.source.Model)
	}

	if strings.Contains(logs.String(), "agent.failover.returned") {
		t.Errorf("preflight announced a recovery it never probed for:\n%s", logs.String())
	}
}

// TestPreflightStillReturnsOnAFreshProbe is the property the fix above must
// not swallow: once the cache entry is gone and the primary really is asked
// again, the release happens exactly as before. A rule that says "only fresh
// facts release a pin" is worthless if nothing ever counts as fresh.
func TestPreflightStillReturnsOnAFreshProbe(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)
	cfg := pinConfig(t, "asked", primaryURL, fallbackURL)
	scope := agentPinScope(cfg, "asked")

	Preflight(t.Context(), cfg, []string{"asked"}, &config.Preflight{})

	primaryUp.Store(false)
	pinServedSource(scope, &cfg.Agents[0], 0)

	primaryUp.Store(true)
	expireProbes()

	Preflight(t.Context(), cfg, []string{"asked"}, &config.Preflight{})

	if _, pinned := selectedSource(scope); pinned {
		t.Error("the agent is still pinned after its primary was asked, and answered")
	}
}

// TestThePreRunProbeRenewsThePinItKeeps is the division of labor between the
// pin's two exit conditions, asserted where they meet.
//
// A pre-run probe that decides to keep a pin has just asked the primary and
// been told it is still unwell — the very fact the lifetime counts down from
// — so the fifteen minutes start again. Without that, a long outage under a
// perfectly healthy preflight would expire the pin on a schedule and send
// failOver back down the list from index 0, demoting an agent that had
// cascaded past two unwell entries onto whichever one answers a single token.
// That is churn produced by the mechanism that exists for the case where
// preflight is not running at all.
func TestThePreRunProbeRenewsThePinItKeeps(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	firstURL, firstUp := togglableProbeEndpoint(t)
	secondURL, _ := togglableProbeEndpoint(t)

	cfg := pinConfigWithFallbacks(t, "renewed", primaryURL,
		pinSource("openai/first", firstURL),
		pinSource("openai/second", secondURL),
	)
	scope := agentPinScope(cfg, "renewed")

	// Pinned where a mid-run cascade would leave it: past an entry that was
	// unwell at the time, on the one that actually served.
	primaryUp.Store(false)
	firstUp.Store(false)
	pinServedSource(scope, &cfg.Agents[0], 1)

	// The outage outlasts a lifetime, with preflight running throughout — and
	// fallback[0] comes back, so a re-walk from index 0 has somewhere to land.
	agePin(t, scope, pinLifetime)
	firstUp.Store(true)
	expireProbes()

	Preflight(t.Context(), cfg, []string{"renewed"}, &config.Preflight{})

	selection, pinned := selectedSource(scope)
	if !pinned {
		t.Fatal("the pin expired while preflight was re-deciding it every run")
	}

	if selection.index != 1 || selection.source.Model != "openai/second" {
		t.Errorf("selection = %q at index %d, want openai/second at index 1 — the expiry walked it back down the list",
			selection.source.Model, selection.index)
	}
}

// TestPreflightReleasesEveryAgentSharingARecoveredPrimary is the freshness
// rule applied to the case that makes it a property of the ANSWER rather than
// of the caller.
//
// Two agents on one model is ordinary — a reviewer and a summarizer over the
// same endpoint — and probeModelCached collapses them to one request on
// purpose. So within a single pass the first agent sends it and the second
// reads it back microseconds later. Counting only the sender as having asked
// stranded every agent but the first on its fallback for the life of the
// process, since keeping a pin also renews pinLifetime: nothing else would
// ever have released it.
func TestPreflightReleasesEveryAgentSharingARecoveredPrimary(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)

	cfg := pinConfig(t, "first", primaryURL, fallbackURL)
	cfg.Agents = append(cfg.Agents, cfg.Agents[0])
	cfg.Agents[1].Name = "second"

	names := []string{"first", "second"}

	primaryUp.Store(false)
	Preflight(t.Context(), cfg, names, &config.Preflight{})

	for _, name := range names {
		if _, pinned := selectedSource(agentPinScope(cfg, name)); !pinned {
			t.Fatalf("%q was not pinned by an outage of the model it runs on", name)
		}
	}

	primaryUp.Store(true)
	expireProbes()
	Preflight(t.Context(), cfg, names, &config.Preflight{})

	for _, name := range names {
		if _, pinned := selectedSource(agentPinScope(cfg, name)); pinned {
			t.Errorf("%q is still pinned after the shared primary answered this very pass", name)
		}
	}
}

// TestPreflightRedecidesWhenTheSharedPinnedSourceDies is the same collapsing,
// in the direction that runs steps at a source preflight has just proved
// dead: one agent's fallback is another's primary, so the probe that finds it
// down belongs to whichever agent preflight reached first. Read as stale, it
// left the pin in place — and every step of the run went to it.
func TestPreflightRedecidesWhenTheSharedPinnedSourceDies(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	sharedURL, sharedUp := togglableProbeEndpoint(t)

	cfg := pinConfig(t, "pinned", primaryURL, sharedURL)
	// The other agent runs ON the first one's fallback, so its probe of that
	// endpoint lands in the cache before the pinned agent asks about it.
	cfg.Agents = append(cfg.Agents, config.Agent{
		Name:     "neighbour",
		Attempts: cfg.Agents[0].Attempts,
		Source:   pinSource("openai/backup", sharedURL),
	})

	scope := agentPinScope(cfg, "pinned")

	primaryUp.Store(false)
	Preflight(t.Context(), cfg, []string{"neighbour", "pinned"}, &config.Preflight{})

	if _, pinned := selectedSource(scope); !pinned {
		t.Fatal("the fallback was not pinned by an outage of the primary")
	}

	// Now the fallback dies too, and the neighbour is the one that finds out.
	sharedUp.Store(false)
	expireProbes()
	Preflight(t.Context(), cfg, []string{"neighbour", "pinned"}, &config.Preflight{})

	if _, pinned := selectedSource(scope); pinned {
		t.Error("the pin survived a fallback this pass had already found dead — every step of the run would go to it")
	}
}

// TestAConcurrentRenewalDoesNotDefeatARelease is why pinRecord keeps its
// clocks OUTSIDE the selection the compare-and-swap guards compare.
//
// clearSourceIf exists to lose a race it should lose: another job's cascade
// pinning a source that has actually served while this pass was probing. With
// a timestamp inside the compared value it also lost races it should WIN — a
// concurrent pass that merely renewed the lifetime changed the compared value
// without changing the source or the index, so a release backed by a fresh
// probe silently did nothing and the agent stayed on the fallback.
func TestAConcurrentRenewalDoesNotDefeatARelease(t *testing.T) {
	scope := testPin("renewed-then-released")
	source := pinSource("openai/backup", "http://127.0.0.1:1/v1/")

	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	selectSource(scope, source, 0)

	record, ok := pinnedSource(scope)
	if !ok {
		t.Fatal("nothing was pinned")
	}

	// Another pass keeps the pin and renews its lifetime, touching neither the
	// source nor the index.
	if !renewPinIf(scope, record.selection) {
		t.Fatal("the renewal did not find the pin it had just read")
	}

	if clearSourceIf(scope, record.selection) != clearedPin {
		t.Error("a release lost to a renewal that changed nothing about which source is pinned")
	}
}

// TestARenewalReportsWhetherThePinWasStillThere is the other half of the same
// guard. reconsiderPin answers keepPin on the strength of this, and a keep
// suppresses BOTH the caller's failover and its failure record — so a renewal
// that quietly did nothing, because a concurrent cascade or an expiry had
// already dropped the pin, produced a green preflight with a dead primary and
// no pin at all.
func TestARenewalReportsWhetherThePinWasStillThere(t *testing.T) {
	scope := testPin("vanished")
	source := pinSource("openai/backup", "http://127.0.0.1:1/v1/")

	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	selectSource(scope, source, 0)

	record, _ := pinnedSource(scope)

	clearSource(scope)

	if renewPinIf(scope, record.selection) {
		t.Error("renewing reported success for a pin that was no longer there")
	}
}

// TestPreflightReportsAnAgentStrandedBetweenTwoDeadSources is the failure the
// freshness rule created and this closes.
//
// Requiring a FRESH fact to release means a cached negative about the pinned
// source can no longer end the pin — correct, since releasing on a stale
// reading destroys what a conversation established. What was wrong was
// reporting that survival as keepPin: the caller reads a keep as "this source
// is serving", and suppresses both the failover and the recorded failure on
// the strength of it. So an agent whose primary was freshly dead and whose
// pinned fallback a probe had already found dead preflighted GREEN, and every
// step of the job ran at an endpoint preflight itself believed was down.
func TestPreflightReportsAnAgentStrandedBetweenTwoDeadSources(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	sharedURL, sharedUp := togglableProbeEndpoint(t)

	cfg := pinConfig(t, "stranded-pair", primaryURL, sharedURL)

	// A neighbour that RUNS on the first agent's fallback, so the probe that
	// finds that endpoint dead is sent by another agent's pass. Two Preflight
	// calls in one pipeline is the ordinary shape: preflight runs per job.
	cfg.Agents = append(cfg.Agents, config.Agent{ //nolint:exhaustruct // only the source is read
		Name:     "neighbour",
		Attempts: cfg.Agents[0].Attempts,
		Source:   pinSource("openai/backup", sharedURL),
	})

	scope := agentPinScope(cfg, "stranded-pair")

	primaryUp.Store(false)
	Preflight(t.Context(), cfg, []string{"stranded-pair"}, &config.Preflight{})

	if _, pinned := selectedSource(scope); !pinned {
		t.Fatal("no fallback was pinned for an agent whose primary is down")
	}

	// Both sources are now down, and the neighbour is who finds out about the
	// fallback.
	sharedUp.Store(false)
	expireProbes()
	Preflight(t.Context(), cfg, []string{"neighbour"}, &config.Preflight{})

	if problems := Preflight(t.Context(), cfg, []string{"stranded-pair"}, &config.Preflight{}); len(problems) == 0 {
		t.Error("preflight reported no problems for an agent whose primary is dead and whose pinned fallback it has already probed dead")
	}
}

// TestPreflightReleasesAnAgentWhoseNeighbourAskedTheQuestion is the ordering
// bug that made a pin permanent.
//
// Preflight runs once per JOB, and several agents legitimately share one
// endpoint and model — the probe cache collapses them to a single request on
// purpose. When freshness meant "this pass sent it", whichever agent's pass
// ran second could never see a fresh positive, so it could never release; and
// because keeping renews the lifetime, it could never expire either. Ordering
// alone stranded it on the fallback for the life of the process, with neither
// agent.failover.returned nor agent.failover.pin_expired ever firing.
func TestPreflightReleasesAnAgentWhoseNeighbourAskedTheQuestion(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, primaryUp := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)

	cfg := pinConfig(t, "early", primaryURL, fallbackURL)
	cfg.Agents = append(cfg.Agents, cfg.Agents[0])
	cfg.Agents[1].Name = "late"

	scopes := map[string]pinScope{
		"early": agentPinScope(cfg, "early"),
		"late":  agentPinScope(cfg, "late"),
	}

	primaryUp.Store(false)

	for name := range scopes {
		Preflight(t.Context(), cfg, []string{name}, &config.Preflight{})
	}

	for name, scope := range scopes {
		if _, pinned := selectedSource(scope); !pinned {
			t.Fatalf("%q was not pinned by an outage of the model it runs on", name)
		}
	}

	// The recovery, seen by two jobs preflighting one after the other.
	primaryUp.Store(true)
	expireProbes()

	Preflight(t.Context(), cfg, []string{"early"}, &config.Preflight{})
	Preflight(t.Context(), cfg, []string{"late"}, &config.Preflight{})

	for name, scope := range scopes {
		if _, pinned := selectedSource(scope); pinned {
			t.Errorf("%q is still pinned after the primary answered — for it, only a neighbour's pass had asked", name)
		}
	}
}

// TestAPinIsScopedByThePipelineIdentityEveryoneElseUses pins WHICH string
// separates two pipelines' pins, which is the half of pinScope nothing tested.
//
// The scope was the Config's file PATH, a second identity that disagreed with
// the one the repo already resolves and publishes. Two costs, both real: a pin
// log line said pipeline=/abs/infra/deploy.yml where the store, /p/<slug> and
// every run_events row said deploy, so correlating a model-quality shift
// against a run record meant knowing the two spellings named one thing; and
// `--name prod=infra/deploy.yml` — an operator saying which pipeline this is —
// moved the store identity and the UI route and left the pin under the path.
//
// Both directions are asserted, because the field has two jobs: two pipelines
// must not share a pin (the collision it was added for), and one pipeline
// under two spellings must not have two (the reason it is an identity rather
// than a path).
func TestAPinIsScopedByThePipelineIdentityEveryoneElseUses(t *testing.T) {
	t.Parallel()

	deploy := &config.Config{Name: "deploy"}   //nolint:exhaustruct // the identity is the field under test
	sameName := &config.Config{Name: "deploy"} //nolint:exhaustruct // loaded from another spelling of one pipeline
	infra := &config.Config{Name: "infra"}     //nolint:exhaustruct // a different pipeline entirely

	scope := agentPinScope(deploy, "reviewer")

	// The string the store's pipelines.name and the web UI's /p/<slug> also
	// carry. A scope spelled any other way cannot be joined to a run record.
	if scope.pipeline != deploy.Name {
		t.Errorf("pin scoped to %q, want the pipeline identity %q", scope.pipeline, deploy.Name)
	}

	if scope != agentPinScope(sameName, "reviewer") {
		t.Error("one pipeline had two pin scopes — an agent pinned under one spelling is read under the other, and neither ever expires the other")
	}

	if scope == agentPinScope(infra, "reviewer") {
		t.Error("two pipelines shared one pin scope: infra's reviewer outage would resolve deploy's reviewer onto an endpoint it never declared")
	}
}
