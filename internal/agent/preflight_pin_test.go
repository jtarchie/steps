package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
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
func pinConfig(t *testing.T, name, primaryURL, fallbackURL string) *config.Config {
	t.Helper()

	t.Cleanup(func() { clearSource(name) })

	return &config.Config{
		Agents: []config.Agent{{
			Name: name,
			//nolint:gosec // an env var NAME, not a credential
			Source: config.AgentSource{Model: "openai/primary", Endpoint: primaryURL, APIKeyEnv: "OPENAI_API_KEY"},
			Fallback: []config.AgentFallback{
				//nolint:gosec // an env var NAME, not a credential
				{Source: config.AgentSource{Model: "openai/backup", Endpoint: fallbackURL, APIKeyEnv: "OPENAI_API_KEY"}},
			},
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

	// The outage: the primary is down, so preflight fails over and pins.
	primaryUp.Store(false)
	ResetProbeCache()

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
	ResetProbeCache()
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

	primaryUp.Store(false)
	ResetProbeCache()
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
}
