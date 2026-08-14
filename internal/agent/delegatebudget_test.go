package agent

import "testing"

// TestDelegatedBudgetDrawsOnTheParent pins the guarantee that makes an agent's
// budget: a bound on its whole delegation subtree rather than on one
// conversation in it: a sub-agent takes a share of what the parent has LEFT,
// and what it spends comes back off the parent's allowance.
func TestDelegatedBudgetDrawsOnTheParent(t *testing.T) {
	t.Parallel()

	t.Run("a share of what remains, capped by the child's own budget", func(t *testing.T) {
		t.Parallel()

		parent := &stepUsage{budget: 1000, delegateFraction: 0.10}

		// 10% of 1000, and the child declares no ceiling of its own.
		if got := parent.delegatedBudget(0); got != 100 {
			t.Errorf("first delegation = %d, want 100 (10%% of 1000 remaining)", got)
		}

		// The child's own budget is tighter, so it wins.
		if got := parent.delegatedBudget(40); got != 40 {
			t.Errorf("delegation with a tighter own budget = %d, want 40", got)
		}

		// The child's own budget is looser, so the parent's share still binds.
		if got := parent.delegatedBudget(900); got != 100 {
			t.Errorf("delegation with a looser own budget = %d, want 100", got)
		}
	})

	t.Run("spending shrinks what later delegations get", func(t *testing.T) {
		t.Parallel()

		parent := &stepUsage{budget: 1000, delegateFraction: 0.10}

		if got := parent.delegatedBudget(0); got != 100 {
			t.Fatalf("first delegation = %d, want 100", got)
		}

		// The child spent its whole 100 and finished, charging the parent.
		parent.chargeDelegated(100)

		// 10% of the 900 left, not of the original 1000 — which is what stops
		// a parent being drained by repeated delegation.
		if got := parent.delegatedBudget(0); got != 90 {
			t.Errorf("second delegation = %d, want 90 (10%% of 900 remaining)", got)
		}
	})

	t.Run("an exhausted parent funds nothing", func(t *testing.T) {
		t.Parallel()

		parent := &stepUsage{budget: 1000, delegateFraction: 0.10}
		parent.chargeDelegated(1000)

		if got := parent.delegatedBudget(500); got != 0 {
			t.Errorf("delegation from a spent parent = %d, want 0 — the child's own budget must not resurrect an exhausted allowance", got)
		}
	})

	t.Run("a parent with no ceiling leaves the child on its own", func(t *testing.T) {
		t.Parallel()

		parent := &stepUsage{delegateFraction: 0.10}

		if got := parent.delegatedBudget(250); got != 250 {
			t.Errorf("delegation from an unbounded parent = %d, want the child's own 250", got)
		}
	})
}

// TestChargeDelegatedWalksTheChain proves the bound is transitive: a
// grandchild's tokens shrink the grandparent's allowance too, or a deep enough
// delegation tree escapes the ceiling one level at a time.
func TestChargeDelegatedWalksTheChain(t *testing.T) {
	t.Parallel()

	grandparent := &stepUsage{budget: 1000, delegateFraction: 0.50}
	parent := &stepUsage{budget: 500, delegateFraction: 0.50, parent: grandparent}
	child := &stepUsage{budget: 250, parent: parent}

	child.chargeDelegated(200)

	if got := parent.remaining(); got != 300 {
		t.Errorf("parent remaining = %d, want 300", got)
	}

	if got := grandparent.remaining(); got != 800 {
		t.Errorf("grandparent remaining = %d, want 800 — a grandchild's spend must reach it", got)
	}
}

// TestDelegatedSpendCountsAgainstTheParentsOwnCeiling covers the other half:
// a step that handed most of its allowance to helpers has that much less for
// its own turns, or the ceiling would bound the parent while the subtree spent
// freely beside it.
func TestDelegatedSpendCountsAgainstTheParentsOwnCeiling(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{budget: 1000}
	usage.chargeDelegated(950)

	if got := usage.remaining(); got != 50 {
		t.Fatalf("remaining = %d, want 50", got)
	}

	// The parent's own spend now crosses the ceiling that its delegations
	// have almost exhausted.
	exceeded := usage.record(response(50, 50))
	if !exceeded {
		t.Error("record reported no breach; delegated spend must count against the step's own ceiling")
	}
}
