package merkle

// The per-set walk against the recursion it replaces.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestPlanChainsSetsMatchRecursion is the upgrade story in one assertion:
// for every shape that exists today — no fan-out, and one every-get — a walk
// per input set produces BYTE-IDENTICAL chains to the recursion. Same nodes,
// same parents, same hashes, same roots. If this ever fails, existing
// pipelines' caches would go cold on upgrade, and that is a stop-the-line
// finding, not a re-run note.
func TestPlanChainsSetsMatchRecursion(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "listing",
			Config: config.ResourceTypeConfig{Check: `printf '[{"n":"1"},{"n":"2"},{"n":"3"}]'`, In: "true"},
		}},
		Resources: []config.Resource{
			{Name: "a", Type: "listing"},
			{Name: "b", Type: "listing"},
		},
	}

	tests := map[string]struct {
		steps []config.Step
		sets  []InputSet
	}{
		"no fan-out": {
			steps: []config.Step{
				{Get: "a"},
				{Get: "b"},
				{Task: "work", Run: "true", Inputs: config.Inputs()},
			},
			sets: []InputSet{{
				"a": {"n": "3"},
				"b": {"n": "3"},
			}},
		},
		"one every-get": {
			steps: []config.Step{
				{Get: "a", Version: "every"},
				{Get: "b"},
				{Task: "work", Run: "true", Inputs: config.Inputs()},
			},
			sets: []InputSet{
				{"a": {"n": "1"}, "b": {"n": "3"}},
				{"a": {"n": "2"}, "b": {"n": "3"}},
				{"a": {"n": "3"}, "b": {"n": "3"}},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()

			recursive, err := PlanChains(ctx, cfg, "build", test.steps, nil, nil, nil)
			if err != nil {
				t.Fatalf("PlanChains (recursion): %v", err)
			}

			perSet, err := PlanChains(ctx, cfg, "build", test.steps, nil, nil, test.sets)
			if err != nil {
				t.Fatalf("PlanChains (sets): %v", err)
			}

			if !reflect.DeepEqual(recursive, perSet) {
				t.Errorf("chains diverge:\nrecursion: %+v\nsets:      %+v", recursive, perSet)
			}
		})
	}
}

// TestPlanChainsUnboundGetIsAnError: a set that binds no version for a get in
// the plan is a contradiction — resolveInputSets binds every one — and the
// planner must say so rather than substitute a version of its own. It used to
// fall back to the NEWEST resolved version while the executor fell back to the
// OLDEST, so any future path reaching this would have had the planner hash one
// version and the executor fetch another: the cache would then record a chain
// as succeeded for work that never ran.
func TestPlanChainsUnboundGetIsAnError(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "listing",
			Config: config.ResourceTypeConfig{Check: `printf '[{"n":"1"},{"n":"2"}]'`, In: "true"},
		}},
		Resources: []config.Resource{{Name: "a", Type: "listing"}},
	}

	steps := []config.Step{{Get: "a"}}

	_, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil, []InputSet{{}})
	if err == nil {
		t.Fatal("PlanChains accepted a set that binds nothing for get \"a\"")
	}
}

// TestPlanChainsEmptySetsPlanNothing: zero sets is "nothing to build", the
// same answer an every-get with no versions gives the recursion.
func TestPlanChainsEmptySetsPlanNothing(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "listing",
			Config: config.ResourceTypeConfig{Check: `printf '[]'`, In: "true"},
		}},
		Resources: []config.Resource{{Name: "a", Type: "listing"}},
	}

	steps := []config.Step{{Get: "a", Version: "every"}}

	chains, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil, []InputSet{})
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 0 {
		t.Errorf("chains = %d, want none", len(chains))
	}
}

// TestPlanChainsResolveArtifactImages: a task whose image: names a get is
// hashed under the digest that get bound, identically by both walks, so each
// version of a version: every get gets its own task hash.
func TestPlanChainsResolveArtifactImages(t *testing.T) {
	t.Parallel()

	one, two := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "digests",
			Config: config.ResourceTypeConfig{Check: fmt.Sprintf(`printf '[{"digest":"%s"},{"digest":"%s"}]'`, one, two), In: "true"},
		}},
		Resources: []config.Resource{{Name: "img", Type: "digests", Source: map[string]any{"repository": "ghcr.io/me/img"}}},
	}

	steps := []config.Step{
		{Get: "img", Version: "every"},
		{Task: "work", Run: "true", Image: "img"},
	}
	sets := []InputSet{{"img": {"digest": one}}, {"img": {"digest": two}}}

	recursive, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil, nil)
	if err != nil {
		t.Fatalf("PlanChains (recursion): %v", err)
	}

	perSet, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil, sets)
	if err != nil {
		t.Fatalf("PlanChains (sets): %v", err)
	}

	if !reflect.DeepEqual(recursive, perSet) {
		t.Fatalf("chains diverge:\nrecursion: %+v\nsets:      %+v", recursive, perSet)
	}

	if len(perSet) != 2 {
		t.Fatalf("chains = %d, want 2", len(perSet))
	}

	for i, digest := range []string{one, two} {
		task := perSet[i].Nodes[1]
		if want := "ghcr.io/me/img@" + digest; task.Content["image"] != want {
			t.Errorf("chain %d task image = %v, want %q", i, task.Content["image"], want)
		}
	}

	if perSet[0].Nodes[1].Hash == perSet[1].Nodes[1].Hash {
		t.Error("two digests hashed one task node")
	}
}

// TestPlanChainsUnresolvableArtifactImageIsUnskippable: the planner does not
// fail a plan over an image it cannot bind — the executor reports that in the
// step's own place — but it must never let such a chain be skipped.
func TestPlanChainsUnresolvableArtifactImageIsUnskippable(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Resources: []config.Resource{{Name: "img", Source: map[string]any{"repository": "r"}}}}

	chains, err := PlanChains(context.Background(), cfg, "build", []config.Step{{Task: "work", Run: "true", Image: "img"}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 1 || !chains[0].Unskippable {
		t.Fatalf("chains = %+v, want one unskippable chain", chains)
	}

	if image := chains[0].Nodes[0].Content["image"]; image != "img" {
		t.Errorf("task image = %v, want the literal name", image)
	}
}
