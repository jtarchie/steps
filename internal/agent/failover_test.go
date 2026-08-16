package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
)

// TestResolveWithFailoverKeepsThePrimaryForHashing is the mechanism behind the
// not-hashed rule: a step hashes against the invocation as CONFIGURED and runs
// against the one that is actually reachable. If those were the same value, an
// outage would move every agent step's cache key at exactly the moment things
// are already going badly.
func TestResolveWithFailoverKeepsThePrimaryForHashing(t *testing.T) {
	ResetProbeCache()

	cfg := &config.Config{Agents: []config.Agent{{
		Name:   "writer",
		System: "you are a writer",
		Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "PRIMARY_KEY"},
		Fallback: []config.AgentFallback{{
			Source: config.AgentSource{Model: "anthropic/claude-sonnet-4-5", APIKeyEnv: "BACKUP_KEY"},
		}},
	}}}

	step := config.Step{Agent: "writer", Prompt: "write it"}

	// Before any failover, the two are the same invocation, and there is no
	// fallback index to resume the mid-run cascade from.
	primary, effective, _, fallbackIndex, err := resolveWithFailover(cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover: %v", err)
	}

	if effective.ModelName != primary.ModelName {
		t.Fatalf("with no failover selected, effective = %q, want the primary %q", effective.ModelName, primary.ModelName)
	}

	assertFallbackIndex(t, fallbackIndex, -1, "still the primary")

	// Preflight selects the fallback.
	selectSource("writer", cfg.Agents[0].Fallback[0].Source, 0)

	primary, effective, _, fallbackIndex, err = resolveWithFailover(cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover after failover: %v", err)
	}

	assertFallbackIndex(t, fallbackIndex, 0, "agent.Fallback[0], the one preflight selected")

	if primary.ModelName != "gpt-4o" {
		t.Errorf("primary model = %q, want the CONFIGURED model — this is what the step hashes as", primary.ModelName)
	}

	if effective.ModelName != "claude-sonnet-4-5" {
		t.Errorf("effective model = %q, want the fallback — this is what actually serves the run", effective.ModelName)
	}

	if effective.APIKeyEnv != "BACKUP_KEY" {
		t.Errorf("effective api_key_env = %q, want the fallback's own credential", effective.APIKeyEnv)
	}

	// An outage changes where requests GO, never what the agent IS.
	if effective.Persona != primary.Persona || effective.MaxTurns != primary.MaxTurns {
		t.Error("failover changed the agent's persona or limits, not just its source")
	}

	// The compaction budget follows the model that will actually serve the
	// conversation: a 1M fallback must not inherit the primary's 128K.
	if effective.ContextWindow != 1_000_000 {
		t.Errorf("effective context window = %d, want the fallback model's 1000000", effective.ContextWindow)
	}
}

// assertFallbackIndex checks resolveWithFailover's reported cascade
// position — where runPreparedWithFailover's mid-run loop would continue
// from.
func assertFallbackIndex(t *testing.T, got, want int, why string) {
	t.Helper()

	if got != want {
		t.Errorf("fallbackIndex = %d, want %d (%s)", got, want, why)
	}
}

// TestEnsembleMembersHashIndependently pins the cost property that makes an
// ensemble affordable to iterate on: each member is its own merkle node, so
// editing one member's prompt re-runs only that member rather than the whole
// panel.
func TestEnsembleMembersHashIndependently(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Agents: []config.Agent{
		{Name: "a", Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "K"}},
		{Name: "b", Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "K"}},
	}}

	member := func(name, prompt string) config.Step {
		return config.Step{Agent: name, Prompt: prompt, Verdicts: []config.VerdictRoute{{Name: "approve"}, {Name: "reject"}}}
	}

	hashOf := func(t *testing.T, step config.Step) string {
		t.Helper()

		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			t.Fatalf("ResolveAgentInvocation: %v", err)
		}

		content, err := merkle.AgentContentMap(cfg, step, ri)
		if err != nil {
			t.Fatalf("AgentContentMap: %v", err)
		}

		hash, err := merkle.HashNode(merkle.NodeKindAgent, content, "")
		if err != nil {
			t.Fatalf("HashNode: %v", err)
		}

		return hash
	}

	untouched := hashOf(t, member("b", "review it"))

	before := hashOf(t, member("a", "review it"))
	after := hashOf(t, member("a", "review it carefully"))

	if before == after {
		t.Error("editing a member's prompt did not change that member's hash")
	}

	if untouched != hashOf(t, member("b", "review it")) {
		t.Error("editing one member changed another member's hash")
	}
}

// TestNextHostedFallback pins the mid-run cascade's advance rule: it walks
// agent.Fallback strictly in order from wherever it is, and a CLI-backed
// candidate stops the cascade there rather than being skipped over — see
// runPreparedWithFailover's doc comment on why CLI sources sit outside this
// mechanism entirely.
func TestNextHostedFallback(t *testing.T) {
	agent := &config.Agent{
		Name: "writer",
		Fallback: []config.AgentFallback{
			{Source: config.AgentSource{Model: "openai/gpt-4o", APIKeyEnv: "K1"}},
			{Source: config.AgentSource{Model: "@claude/sonnet"}},
			{Source: config.AgentSource{Model: "anthropic/claude-sonnet-4-5", APIKeyEnv: "K2"}},
		},
	}

	var ri config.ResolvedInvocation

	next, nextIndex, ok := nextHostedFallback(agent, ri, -1)
	if !ok || nextIndex != 0 || next.ModelName != "gpt-4o" {
		t.Fatalf("nextHostedFallback(-1) = (%+v, %d, %v), want index 0, gpt-4o, true", next, nextIndex, ok)
	}

	// Index 1 is a CLI source: the cascade stops rather than skipping past it
	// to index 2.
	_, _, ok = nextHostedFallback(agent, ri, 0)
	if ok {
		t.Error("nextHostedFallback(0) = ok, want false — a CLI candidate must stop the mid-run cascade, not be skipped")
	}

	// Past the end of the list.
	_, _, ok = nextHostedFallback(agent, ri, 2)
	if ok {
		t.Error("nextHostedFallback(2) = ok, want false — no more fallback: entries")
	}
}

// TestNextViableFallbackSkipsAMissingCredential is the regression for the
// cascade giving up on the WHOLE ordered list over one bad entry: unlike a
// CLI candidate (which the cascade stops at unconditionally — see
// nextHostedFallback), a hosted candidate whose api_key_env isn't set in
// this environment is a config problem on THAT one entry, not a reason to
// abandon a healthy entry further down the same list — the same "try the
// next one" treatment preflight's own failOver already gives an unhealthy
// candidate.
func TestNextViableFallbackSkipsAMissingCredential(t *testing.T) {
	t.Setenv("STEPS_TEST_NEXTVIABLE_K2", "present")

	agent := &config.Agent{
		Name: "writer",
		Fallback: []config.AgentFallback{
			{Source: config.AgentSource{Model: "openai/gpt-4o-mini", APIKeyEnv: "STEPS_TEST_NEXTVIABLE_UNSET"}},
			{Source: config.AgentSource{Model: "anthropic/claude-sonnet-4-5", APIKeyEnv: "STEPS_TEST_NEXTVIABLE_K2"}},
		},
	}

	var ri config.ResolvedInvocation

	next, apiKey, nextIndex, ok := nextViableFallback(agent, ri, -1)
	if !ok {
		t.Fatal("nextViableFallback(-1) = false, want true — index 1's credential is present, so it must not be walled off by index 0's missing one")
	}

	if nextIndex != 1 || next.ModelName != "claude-sonnet-4-5" {
		t.Errorf("nextViableFallback(-1) = (model %q, index %d), want (claude-sonnet-4-5, 1) — index 0 must be skipped, not selected or fatal", next.ModelName, nextIndex)
	}

	if apiKey != "present" {
		t.Errorf("apiKey = %q, want the resolved candidate's own key %q", apiKey, "present")
	}

	// Every candidate's credential missing: nothing left to select.
	agentAllMissing := &config.Agent{
		Name: "writer",
		Fallback: []config.AgentFallback{
			{Source: config.AgentSource{Model: "openai/gpt-4o-mini", APIKeyEnv: "STEPS_TEST_NEXTVIABLE_UNSET"}},
		},
	}

	_, _, _, ok = nextViableFallback(agentAllMissing, ri, -1)
	if ok {
		t.Error("nextViableFallback with every candidate's credential missing = true, want false")
	}
}

// TestFailoverEligible pins what the mid-run cascade reacts to: a plain
// infrastructure error carrying a retryable status, and nothing else — not a
// nil error, not a task-level failure (outcome.Fail), not a canceled or
// deadline-exceeded context (even one that happens to wrap a 5xx-shaped error
// underneath), and not an internal error this package raises itself (a
// budget breach classifies as outcome.Errored too, but is not the
// provider's fault).
func TestFailoverEligible(t *testing.T) {
	ctx := context.Background()

	// The shape a real connection failure takes reaching here (via
	// net/http's client) — a bare errors.New with dial-shaped TEXT is not a
	// net.Error and must not pass isTransientProviderError.
	dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	tests := []struct {
		name string
		ctx  context.Context //nolint:containedctx // table-driven: each case needs its own context (background vs. canceled)
		err  error
		want bool
	}{
		{"nil error", ctx, nil, false},
		{"5xx api error", ctx, &openai.Error{StatusCode: http.StatusInternalServerError}, true},
		{"400 api error", ctx, &openai.Error{StatusCode: http.StatusBadRequest}, false},
		{"task-level failure wrapping a 5xx shape", ctx, outcome.Fail(&openai.Error{StatusCode: http.StatusInternalServerError}), false},
		{"plain connection error", ctx, dialErr, true},
		{"budget exceeded is errored but not the provider's fault", ctx, errors.New("agent budget exceeded (spent 500 tokens)"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failoverEligible(tt.ctx, tt.err); got != tt.want {
				t.Errorf("failoverEligible(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := failoverEligible(canceledCtx, &openai.Error{StatusCode: http.StatusInternalServerError}); got {
		t.Error("failoverEligible on a canceled context = true, want false — a canceled run must not cascade")
	}
}

// TestRemainingTurnsSpansTheCascade pins max_turns: as a ceiling on the STEP
// rather than on each source it is tried on.
//
// Without this a declared cap of 30 permits 30 turns per fallback entry: the
// resumed conversation carries its history forward but restarted its own turn
// count, so the ceiling silently multiplied by the length of the list. cli.go's
// session rejoin already subtracts turns already spent; this is the hosted
// cascade's equivalent.
func TestRemainingTurnsSpansTheCascade(t *testing.T) {
	t.Parallel()

	conv := agentConversation{maxTurns: 30} //nolint:exhaustruct // only the ceiling is under test

	if got := remainingTurns(conv, 0); got != 30 {
		t.Errorf("a fresh conversation gets %d turns, want the full 30", got)
	}

	if got := remainingTurns(conv, 25); got != 5 {
		t.Errorf("after 25 turns the next source gets %d, want the remaining 5", got)
	}

	// A checkpoint at or past the ceiling yields no turns rather than a
	// negative count, so the loop falls straight through to outOfTurns.
	if got := remainingTurns(conv, 30); got != 0 {
		t.Errorf("a spent budget gives %d turns, want 0", got)
	}

	if got := remainingTurns(conv, 42); got != 0 {
		t.Errorf("an overspent budget gives %d turns, want 0 (never negative)", got)
	}
}

// TestSeedResumeStateCarriesTheWholeCheckpoint proves every field a swap has
// to remember actually survives into the next source's bookkeeping.
//
// Each of these was a real bug when it was missing: a required tool firing its
// side effect twice, a budgeted tool getting a fresh allowance per source, a
// verdict the primary had already decided going missing, and a compaction
// summary re-derived from a history that already contained one.
func TestSeedResumeStateCarriesTheWholeCheckpoint(t *testing.T) {
	t.Parallel()

	conv := agentConversation{ //nolint:exhaustruct // the checkpoint is what is under test
		resume: &resumeCheckpoint{
			satisfied:  map[string]bool{"post_review": true},
			callCounts: map[string]int{"post_review": 1},
			trajectory: []recordedToolCall{{name: "post_review"}},
			verdict:    "approve",
			note:       "looks right",
			turnsSpent: 7,
			summary:    "the story so far",
			stalled:    true,
		},
	}

	state := seedResumeState(conv)

	if !state.satisfied["post_review"] {
		t.Error("a required tool already satisfied would be forced to fire again")
	}

	if state.callCounts["post_review"] != 1 {
		t.Errorf("callCounts = %v, want the spend carried forward", state.callCounts)
	}

	if len(state.trajectory) != 1 {
		t.Errorf("trajectory = %v, want the prior source's calls kept", state.trajectory)
	}

	if state.verdict != "approve" || state.note != "looks right" {
		t.Errorf("verdict/note = %q/%q, want the decision already made", state.verdict, state.note)
	}

	if state.turnsSpent != 7 {
		t.Errorf("turnsSpent = %d, want 7 — the ceiling spans the cascade", state.turnsSpent)
	}

	if state.summary != "the story so far" || !state.stalled {
		t.Errorf("compaction state = %q/%v, want it carried so the fallback does not summarize a summary", state.summary, state.stalled)
	}
}

// TestSeedResumeStateStartsFreshWithoutACheckpoint keeps the first attempt
// behaving exactly as it did before resuming existed.
func TestSeedResumeStateStartsFreshWithoutACheckpoint(t *testing.T) {
	t.Parallel()

	state := seedResumeState(agentConversation{}) //nolint:exhaustruct // a first attempt carries nothing

	if state.satisfied == nil || state.callCounts == nil {
		t.Error("a fresh conversation needs usable (non-nil) bookkeeping maps")
	}

	if state.turnsSpent != 0 || state.verdict != "" || len(state.trajectory) != 0 {
		t.Errorf("a fresh conversation started from %+v, want zero", state)
	}
}

// TestPinnedSourceOnlyAfterServing is the difference between preferring a
// source that WORKED and one that was merely chosen.
//
// Pinning on selection stranded the process: a cascade that then exhausted
// every source left the pin pointing at something that never answered, nothing
// un-pinned it, and preflight only ever probes the PRIMARY — so no later run
// re-examined the choice for the life of the process.
func TestPinnedSourceOnlyAfterServing(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	agent := &config.Agent{ //nolint:exhaustruct // only the fallback list is read
		Name: "writer",
		Fallback: []config.AgentFallback{
			{Source: config.AgentSource{Model: "backup-a"}},
			{Source: config.AgentSource{Model: "backup-b"}},
		},
	}

	// The primary serving pins nothing: it is the default already.
	pinServedSource(agent, -1)

	if _, pinned := selectedSource("writer"); pinned {
		t.Error("the primary serving must not pin anything")
	}

	pinServedSource(agent, 1)

	selection, pinned := selectedSource("writer")
	if !pinned {
		t.Fatal("a fallback that served was not pinned")
	}

	if selection.index != 1 || selection.source.Model != "backup-b" {
		t.Errorf("pinned %+v, want fallback index 1 (backup-b)", selection)
	}

	// Nothing served: the pin goes, so the next run resolves from the primary
	// rather than preferring a source that just failed.
	releaseSource(agent)

	if _, pinned := selectedSource("writer"); pinned {
		t.Error("an exhausted cascade left a pin behind — the next run would prefer a dead source")
	}
}

// TestSelectedSourceIndexSurvivesDuplicateSources is why the selection stores
// its own position rather than searching the list for a matching source.
//
// Two entries may legitimately hold the same source — a key rotation, or an
// honest copy-paste. A value scan reports the FIRST match, so a cascade
// continuing from it would re-try the very source whose failure triggered it.
func TestSelectedSourceIndexSurvivesDuplicateSources(t *testing.T) {
	ResetProbeCache()
	t.Cleanup(ResetProbeCache)

	shared := config.AgentSource{Model: "same-model"}

	agent := &config.Agent{ //nolint:exhaustruct // only the fallback list is read
		Name:     "writer",
		Fallback: []config.AgentFallback{{Source: shared}, {Source: shared}},
	}

	pinServedSource(agent, 1)

	selection, pinned := selectedSource("writer")
	if !pinned {
		t.Fatal("nothing was pinned")
	}

	if selection.index != 1 {
		t.Errorf("selection index = %d, want 1 — a value scan would have reported 0 and re-tried the dead source", selection.index)
	}
}
