package merkle

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func taskHashWithRouting(t *testing.T, step config.Step) string {
	t.Helper()

	cfg := &config.Config{}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	content, err := TaskNodeContent(cfg, step, rt)
	if err != nil {
		t.Fatalf("TaskNodeContent: %v", err)
	}

	hash, err := HashNode(NodeKindTask, content, "")
	if err != nil {
		t.Fatalf("HashNode: %v", err)
	}

	return hash
}

// TestRoutingOmittedFromHashWhenUnset proves value-gating: a step with no
// routing hashes byte-identically to before these fields existed.
func TestRoutingOmittedFromHashWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	step := config.Step{Task: "work", Run: "make"}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	content, err := TaskNodeContent(cfg, step, rt)
	if err != nil {
		t.Fatalf("TaskNodeContent: %v", err)
	}

	for _, key := range []string{"to", "max_visits", "verdicts"} {
		if _, present := content[key]; present {
			t.Errorf("unset routing must not put %q in the hashed content", key)
		}
	}
}

func TestRoutingBustsTaskHash(t *testing.T) {
	t.Parallel()

	base := taskHashWithRouting(t, config.Step{Task: "work", Run: "make"})
	withTo := taskHashWithRouting(t, config.Step{Task: "work", Run: "make", To: map[string]string{"failure": "work"}, MaxVisits: 3})
	changedTarget := taskHashWithRouting(t, config.Step{Task: "work", Run: "make", To: map[string]string{"failure": "other"}, MaxVisits: 3})
	changedVisits := taskHashWithRouting(t, config.Step{Task: "work", Run: "make", To: map[string]string{"failure": "work"}, MaxVisits: 5})

	pairs := []struct {
		name string
		a, b string
	}{
		{"adding to:", base, withTo},
		{"changing a target", withTo, changedTarget},
		{"changing max_visits", withTo, changedVisits},
	}

	for _, p := range pairs {
		if p.a == p.b {
			t.Errorf("%s should change the hash, but hashes matched", p.name)
		}
	}
}

// TestVerdictsBustAgentHash proves the verdict vocabulary (which changes the
// synthesized tool set) folds into the agent hash, including a reorder.
func TestVerdictsBustAgentHash(t *testing.T) {
	t.Parallel()

	cfg := agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "")

	base := mustAgentHash(t, cfg, config.Step{Agent: "reviewer", Prompt: "x"})
	withVerdicts := mustAgentHash(t, cfg, config.Step{
		Agent: "reviewer", Prompt: "x",
		Verdicts:  []string{"approve", "revise"},
		To:        map[string]string{"approve": "reviewer", "revise": "reviewer"},
		MaxVisits: 2,
	})
	reordered := mustAgentHash(t, cfg, config.Step{
		Agent: "reviewer", Prompt: "x",
		Verdicts:  []string{"revise", "approve"},
		To:        map[string]string{"approve": "reviewer", "revise": "reviewer"},
		MaxVisits: 2,
	})

	if base == withVerdicts {
		t.Error("declaring verdicts should change the agent hash")
	}

	if withVerdicts == reordered {
		t.Error("reordering verdicts (an enum) should change the hash")
	}
}

// TestRoutingChainUnskippable proves a plan containing a to: step comes back
// Unskippable from PlanChains (the plan-time hardening).
func TestRoutingChainUnskippable(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	steps := []config.Step{
		{Task: "loop", Run: "true", To: map[string]string{"failure": "loop"}, MaxVisits: 2},
	}

	chains, err := PlanChains(context.Background(), cfg, "j", steps, nil)
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 1 {
		t.Fatalf("len(chains) = %d, want 1", len(chains))
	}

	if !chains[0].Unskippable {
		t.Error("a chain containing a to: step must be Unskippable at plan time")
	}
}
