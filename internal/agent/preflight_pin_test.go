package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
// a `steps watch` agent to a possibly-worse model for days — silently, since
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

	selection, pinned := selectedSource("returner")
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

	if _, stillPinned := selectedSource("returner"); stillPinned {
		t.Error("the agent is still pinned to the fallback after its primary recovered")
	}

	// The event name is the contract docs/agents.md publishes, and the only
	// channel this edge has — nothing about a preflight-boundary release
	// reaches a step's output line or the recorded result.
	if !strings.Contains(logs.String(), "agent.failover.returned") {
		t.Errorf("the return was silent:\n%s", logs.String())
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

	if _, pinned := selectedSource("stayer"); !pinned {
		t.Fatal("no fallback was pinned for an agent whose primary is down")
	}

	// A second run, primary still down.
	expireProbes()
	Preflight(t.Context(), cfg, []string{"stayer"}, &config.Preflight{})

	selection, pinned := selectedSource("stayer")
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

	if _, pinned := selectedSource("stranded"); !pinned {
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

	if _, pinned := selectedSource("stranded"); pinned {
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
	selectSource("demoted", cfg.Agents[0].Fallback[1].Source, 1)

	Preflight(t.Context(), cfg, []string{"demoted"}, &config.Preflight{})

	selection, pinned := selectedSource("demoted")
	if !pinned {
		t.Fatal("the pin was dropped while the primary was down and the pinned fallback still serving")
	}

	if selection.index != 1 || selection.source.Model != "openai/second" {
		t.Errorf("selection = %q at index %d, want openai/second at index 1",
			selection.source.Model, selection.index)
	}
}
