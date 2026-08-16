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
	primary, effective, fallbackIndex, err := resolveWithFailover(cfg, step)
	if err != nil {
		t.Fatalf("resolveWithFailover: %v", err)
	}

	if effective.ModelName != primary.ModelName {
		t.Fatalf("with no failover selected, effective = %q, want the primary %q", effective.ModelName, primary.ModelName)
	}

	assertFallbackIndex(t, fallbackIndex, -1, "still the primary")

	// Preflight selects the fallback.
	selectSource("writer", cfg.Agents[0].Fallback[0].Source)

	primary, effective, fallbackIndex, err = resolveWithFailover(cfg, step)
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
