package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/agent/llmclient"
	"github.com/jtarchie/steps/internal/config"
)

func lostKeyConfig(t *testing.T, name string) *config.Config {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")

	primaryURL, _ := togglableProbeEndpoint(t)
	fallbackURL, _ := togglableProbeEndpoint(t)
	cfg := pinConfig(t, name, primaryURL, fallbackURL)
	cfg.Agents[0].Source.APIKeyEnv = "PRIMARY_KEY"
	cfg.Agents[0].Source.Secrets = map[string]string{"PRIMARY_KEY": "((ci:primary_key))"}

	return cfg
}

func lapsedLogin(context.Context, string, string) (string, error) {
	return "", errors.New("op: not signed in")
}

// A primary key that fails to fetch from its secret source (a lapsed login) fails this run over, but pins nothing: the next run, login fixed, is back on the primary.
func TestALostKeyFailsOverForThisRunOnly(t *testing.T) {
	cfg := lostKeyConfig(t, "keyless")
	ctx := WithRunBoundary(WithKeyLookup(t.Context(), "", lapsedLogin))

	if problems := Preflight(ctx, cfg, []string{"keyless"}, &config.Preflight{}); len(problems) != 0 {
		t.Fatalf("problems = %+v, want the fallback to carry this run", problems)
	}

	if record, pinned := pinnedSource(testPin("keyless")); pinned {
		t.Fatalf("preflight pinned %+v on a key fetch failure", record.selection)
	}

	resolved, err := resolveWithFailover(ctx, cfg, config.Step{Agent: "keyless"})
	if err != nil {
		t.Fatalf("resolveWithFailover: %v", err)
	}

	assertFallbackIndex(t, resolved.fallbackIndex, 0, "this run's fallback")

	_, served, err := runPreparedWithFailover(ctx, preparedAgentStep{
		resolvedAgent: resolved,
		pin:           testPin("keyless"),
		llm:           llmclient.New(resolved.ri, "test-key"),
		conv:          agentConversation{system: "system", messages: []string{"prompt"}, maxTurns: testMaxTurns},
	})
	if err != nil || served.ri.ModelName != "backup" {
		t.Fatalf("served %q, err %v; want the fallback to serve", served.ri.ModelName, err)
	}

	if record, pinned := pinnedSource(testPin("keyless")); pinned {
		t.Fatalf("serving on the fallback pinned %+v; the primary's health was never in question", record.selection)
	}

	fixed := func(context.Context, string, string) (string, error) { return "primary-key", nil }
	next := WithRunBoundary(WithKeyLookup(t.Context(), "", fixed))

	resolved, err = resolveWithFailover(next, cfg, config.Step{Agent: "keyless"})
	if err != nil {
		t.Fatalf("resolveWithFailover: %v", err)
	}

	assertFallbackIndex(t, resolved.fallbackIndex, -1, "the primary, login fixed")
}

// A poll has no run to steer: a lost key with a healthy fallback reports nothing and pins nothing.
func TestALostKeyOutsideARunPinsNothing(t *testing.T) {
	cfg := lostKeyConfig(t, "polled")
	ctx := WithKeyLookup(t.Context(), "", lapsedLogin)

	if problems := Preflight(ctx, cfg, []string{"polled"}, &config.Preflight{}); len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}

	if _, pinned := pinnedSource(testPin("polled")); pinned {
		t.Fatal("a poll pinned a fallback on a key fetch failure")
	}
}
