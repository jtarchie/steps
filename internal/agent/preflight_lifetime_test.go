package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

func lifetimeSource(model string) config.AgentSource {
	//nolint:gosec // an env var NAME, not a credential
	return config.AgentSource{Model: model, Endpoint: "http://example.invalid/v1/", APIKeyEnv: "OPENAI_API_KEY"}
}

// TestAPinIsReleasedAtARunBoundaryNotMidJob pins the invariant docs/agents.md
// states: one job, one settled source, throughout.
//
// The lifetime was evaluated on every READ, and a read happens per agent
// STEP. A job of six agent steps a few minutes apart can cross fifteen
// minutes between steps 4 and 5, so the pin was deleted mid-job and the
// remaining steps resolved to a primary that was still down — each paying a
// full attempts: cycle before cascading and re-pinning. Nothing renews
// mid-job: renewPinIf is reachable only from Preflight, which runs once per
// job and is exactly what --no-preflight switches off.
func TestAPinIsReleasedAtARunBoundaryNotMidJob(t *testing.T) {
	ResetProbeCache()

	scope := testPin("longjob")
	selectSource(scope, lifetimeSource("openai/backup"), 0)

	record, ok := pinnedSource(scope)
	if !ok {
		t.Fatal("selectSource recorded nothing")
	}

	jobStart := record.decided.Add(time.Minute)

	// Two steps of ONE job: the second resolves long after the lifetime would
	// have fired on wall-clock, and must still see the same source.
	for i, at := range []time.Time{jobStart, jobStart} {
		if _, pinned := selectedSourceAt(scope, at); !pinned {
			t.Fatalf("step %d of one job lost the pin; a run decides before it starts", i+1)
		}
	}

	// The NEXT run is where it goes, because that is a boundary at which
	// nothing is half-done.
	nextRun := record.decided.Add(pinLifetime + time.Minute)
	if _, pinned := selectedSourceAt(scope, nextRun); pinned {
		t.Fatal("a pin outlived its lifetime across a run boundary")
	}
}

// TestRunBoundaryRidesOnTheContext is the wiring half: without a boundary the
// answer must still be time.Now(), so every non-pipeline caller and every
// test keeps the behavior it had.
func TestRunBoundaryRidesOnTheContext(t *testing.T) {
	before := time.Now()

	if at := runInstant(context.Background()); at.Before(before) {
		t.Fatal("a context with no run boundary should read as now")
	}

	ctx := WithRunBoundary(context.Background())

	first := runInstant(ctx)
	if first.Before(before) {
		t.Fatal("WithRunBoundary stored nothing")
	}

	if second := runInstant(ctx); !second.Equal(first) {
		t.Fatalf("the boundary moved between reads: %v then %v", first, second)
	}
}

// TestACascadeThatMovedGetsAFreshClock covers the second edge.
//
// selectSource preserved both clocks from any existing entry. That is right
// when the SAME source is re-pinned — using a fallback says nothing about the
// primary, and every serving step re-pins, so restarting the lifetime there
// would mean a busy pipeline never re-decides. It is wrong when the cascade
// MOVED: a source that just carried a whole conversation was installed with a
// clock that could already be spent, and dropped on the next read.
//
// docs/agents.md promises one attempts: cycle at most once per fifteen
// minutes per agent; inheriting the clock could cost two within seconds.
func TestACascadeThatMovedGetsAFreshClock(t *testing.T) {
	ResetProbeCache()

	scope := testPin("mover")
	selectSource(scope, lifetimeSource("openai/backup-0"), 0)

	// Age the pin to the edge of its life, as a long outage would.
	nearlySpent := time.Now().Add(-pinLifetime + 10*time.Second)
	ageThePin(t, scope, nearlySpent)

	// The cascade moves on: fallback[0] died mid-conversation and fallback[1]
	// finished the step.
	selectSource(scope, lifetimeSource("openai/backup-1"), 1)

	record, ok := pinnedSource(scope)
	if !ok {
		t.Fatal("the moved pin is gone")
	}

	if time.Since(record.decided) > time.Minute {
		t.Errorf("a source that just served inherited a clock %v old", time.Since(record.decided))
	}

	// since is the bar a probe must clear to speak to the PRIMARY's outage,
	// and moving between fallbacks says nothing about the primary.
	if !record.since.Equal(nearlySpent) {
		t.Errorf("since moved to %v; the primary's outage began at %v", record.since, nearlySpent)
	}
}

// TestARePinOfTheSameSourceKeepsItsClock is the guard on the other side: the
// reason both clocks were preserved in the first place. Every step that runs
// on a pinned fallback and serves re-pins it, so restarting the lifetime here
// would mean a busy pipeline never re-decides at all.
func TestARePinOfTheSameSourceKeepsItsClock(t *testing.T) {
	ResetProbeCache()

	scope := testPin("stayer")
	source := lifetimeSource("openai/backup-0")
	selectSource(scope, source, 0)

	aged := time.Now().Add(-pinLifetime + 10*time.Second)
	ageThePin(t, scope, aged)

	selectSource(scope, source, 0)

	record, _ := pinnedSource(scope)
	if !record.decided.Equal(aged) {
		t.Errorf("re-pinning the same source restarted the lifetime (%v); a busy pipeline would never re-decide", record.decided)
	}
}

func ageThePin(t *testing.T, scope pinScope, to time.Time) {
	t.Helper()

	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	record := selectedSources.by[scope]
	record.since, record.decided = to, to
	selectedSources.by[scope] = record
}

// TestLosingTheClearRaceKeepsTheWinnersPin covers the fourth edge.
//
// When clearSourceIf correctly loses to a concurrent writer, reconsiderPin
// returned false — so keepPin was false, and with the primary probe having
// failed the very next statement was failOver, which walks agent.Fallback
// from index 0 and pins the first entry that answers a one-token probe. The
// guard protected the pin from a stale delete and the fall-through then
// overwrote it anyway, demoting the agent off the entry it had cascaded onto.
//
// The concurrent cascade is staged inside the probe handler, which is exactly
// where the real window is: reconsiderPin's decision spans that request.
func TestLosingTheClearRaceKeepsTheWinnersPin(t *testing.T) {
	ResetProbeCache()
	t.Setenv("OPENAI_API_KEY", "test")

	scope := testPin("raced")
	winner := lifetimeSource("openai/winner")

	var pinnedDuringProbe atomic.Bool

	// The pinned fallback's probe: while it is in flight, another job's
	// cascade lands a source that has actually SERVED.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if pinnedDuringProbe.CompareAndSwap(false, true) {
			selectSource(scope, winner, 1)
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	loser := lifetimeSource("openai/loser")
	loser.Endpoint = server.URL + "/v1/"
	selectSource(scope, loser, 0)

	agent := &config.Agent{
		Name:     "raced",
		Source:   lifetimeSource("openai/primary"),
		Fallback: []config.AgentFallback{{Source: loser}, {Source: winner}},
	}

	primary, err := (&config.Config{Agents: []config.Agent{*agent}}).ResolveAgentInvocation(
		config.Step{Agent: "raced"},
	)
	if err != nil {
		t.Fatalf("resolving the primary: %v", err)
	}

	keep := reconsiderPin(context.Background(), scope, agent, primary,
		&config.Preflight{}, time.Now(), io.EOF)

	if !pinnedDuringProbe.Load() {
		t.Skip("the pinned fallback was never probed, so there was no race to lose")
	}

	if !keep {
		t.Error("losing the compare-and-delete reported no pin, which sends the caller straight into failOver from index 0")
	}

	record, ok := pinnedSource(scope)
	if !ok || record.selection.source.Model != "openai/winner" {
		t.Errorf("pin = %+v, want the concurrent winner left in place", record.selection.source.Model)
	}
}
