package merkle

import (
	"context"
	"errors"
	"testing"

	rsrc "github.com/jtarchie/steps/internal/resource"
)

// checks resolves gets the way the executor does — by running each resource
// type's check command — memoizing nothing. Only tests may reach resource;
// depguard keeps it out of merkle proper.
var checks VersionResolver = (*rsrc.Cache)(nil)

// TestPlanChainsRefusesAGetWithNoResolver: a nil interface cannot run a check, and a planner that guessed a version would hash a chain the executor never runs.
func TestPlanChainsRefusesAGetWithNoResolver(t *testing.T) {
	t.Parallel()

	_, err := PlanChains(context.Background(), planCfg(`[{"ref":"v1"}]`, "v1"), "build", testPlanSteps("true"), nil, nil, nil)
	if !errors.Is(err, errNoResolver) {
		t.Fatalf("PlanChains err = %v, want errNoResolver", err)
	}
}
