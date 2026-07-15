package merkle

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func planCfg(checkOutput, sourceVal string) *config.Config {
	return &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name: "dummy",
			Config: config.ResourceTypeConfig{
				Check: "echo '" + checkOutput + "'",
				In:    "true",
			},
		}},
		Resources: []config.Resource{{
			Name:   "thing",
			Type:   "dummy",
			Source: map[string]any{"key": sourceVal},
		}},
	}
}

func testPlanSteps(run string) []config.Step {
	return []config.Step{
		{Get: "thing"},
		{Task: "work", Run: run},
	}
}

// mustPlanRootHash plans steps against cfg and returns the single resulting
// chain's root hash, failing the test on error or an unexpected chain count.
func mustPlanRootHash(t *testing.T, cfg *config.Config, steps []config.Step) string {
	t.Helper()

	chains, err := PlanChains(context.Background(), cfg, "build", steps, nil)
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 1 {
		t.Fatalf("len(chains) = %d, want 1", len(chains))
	}

	return chains[0].RootHash
}

func TestPlanChainsHashDeterminism(t *testing.T) {
	t.Parallel()

	base := planCfg(`[{"ref":"v1"}]`, "v1")
	baseHash := mustPlanRootHash(t, base, testPlanSteps("echo hi"))

	repeatHash := mustPlanRootHash(t, base, testPlanSteps("echo hi"))
	if repeatHash != baseHash {
		t.Errorf("identical inputs produced different root hashes: %q != %q", baseHash, repeatHash)
	}

	taskChangedHash := mustPlanRootHash(t, base, testPlanSteps("echo bye"))
	if taskChangedHash == baseHash {
		t.Error("changing the task run script did not change the root hash")
	}

	sourceChanged := planCfg(`[{"ref":"v1"}]`, "v2")

	sourceChangedHash := mustPlanRootHash(t, sourceChanged, testPlanSteps("echo hi"))
	if sourceChangedHash == baseHash {
		t.Error("changing the resource source did not change the root hash")
	}

	versionChanged := planCfg(`[{"ref":"v2"}]`, "v1")

	versionChangedHash := mustPlanRootHash(t, versionChanged, testPlanSteps("echo hi"))
	if versionChangedHash == baseHash {
		t.Error("changing the resolved get version did not change the root hash")
	}
}

func TestPlanChainsVersionEveryFansOut(t *testing.T) {
	t.Parallel()

	cfg := planCfg(`[{"ref":"v1"},{"ref":"v2"}]`, "v1")
	steps := []config.Step{{Get: "thing", Version: "every"}}

	chains, err := PlanChains(context.Background(), cfg, "build", steps, nil)
	if err != nil {
		t.Fatalf("PlanChains: %v", err)
	}

	if len(chains) != 2 {
		t.Fatalf("len(chains) = %d, want 2", len(chains))
	}

	if chains[0].RootHash == chains[1].RootHash {
		t.Error("distinct versions produced the same root hash")
	}
}

// TestTaskNodeContentOmitsInputsOutputsWithoutWorkspace guards the
// cache-stability guarantee workspace.go's taskNodeContent doc comment
// promises: a pipeline that never sets workspace: must hash exactly as it
// did before inputs:/outputs: existed, so this feature can never silently
// invalidate anyone's existing cache.
func TestTaskNodeContentOmitsInputsOutputsWithoutWorkspace(t *testing.T) {
	t.Parallel()

	rt := config.ResolvedTask{Name: "build", Run: "echo hi", Inputs: []string{"repo"}, Outputs: []string{"built"}}

	content := TaskNodeContent(rt, nil)
	if len(content) != 1 {
		t.Fatalf(`TaskNodeContent(rt, nil) = %#v, want exactly {"run": ...}, no inputs/outputs keys`, content)
	}

	if content["run"] != rt.Run {
		t.Errorf(`content["run"] = %v, want %v`, content["run"], rt.Run)
	}
}

// TestTaskNodeContentInputsOutputsOnlyAffectHashWhenWorkspaceConfigured is
// the golden-hash-style regression: declaring inputs/outputs must be inert
// for the hash when no workspace: block is configured, and load-bearing
// once one is.
func TestTaskNodeContentInputsOutputsOnlyAffectHashWhenWorkspaceConfigured(t *testing.T) {
	t.Parallel()

	base := config.ResolvedTask{Name: "build", Run: "echo hi"}
	withIO := config.ResolvedTask{Name: "build", Run: "echo hi", Inputs: []string{"repo"}, Outputs: []string{"built"}}

	mustHash := func(t *testing.T, rt config.ResolvedTask, ws *config.WorkspaceConfig) string {
		t.Helper()

		hash, err := HashNode(NodeKindTask, TaskNodeContent(rt, ws), "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	withoutWorkspaceBase := mustHash(t, base, nil)
	withoutWorkspaceIO := mustHash(t, withIO, nil)

	if withoutWorkspaceBase != withoutWorkspaceIO {
		t.Error("declaring inputs/outputs changed the hash even though no workspace: block is configured")
	}

	ws := &config.WorkspaceConfig{Strategy: "copy"}

	withWorkspaceBase := mustHash(t, base, ws)
	withWorkspaceIO := mustHash(t, withIO, ws)

	if withWorkspaceBase == withWorkspaceIO {
		t.Error("declaring inputs/outputs did not change the hash when a workspace: block is configured")
	}
}

// TestPutNodeContentInputsOnlyAffectHashWhenWorkspaceConfigured mirrors the
// task case above for put steps.
func TestPutNodeContentInputsOnlyAffectHashWhenWorkspaceConfigured(t *testing.T) {
	t.Parallel()

	rt := config.ResourceType{Config: config.ResourceTypeConfig{Out: "true"}}
	source := map[string]any{"key": "v"}

	mustHash := func(t *testing.T, inputs []string, ws *config.WorkspaceConfig) string {
		t.Helper()

		hash, err := HashNode(NodeKindPut, PutNodeContent(rt, source, nil, inputs, ws), "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	if mustHash(t, nil, nil) != mustHash(t, []string{"built"}, nil) {
		t.Error("declaring inputs changed a put node's hash even though no workspace: block is configured")
	}

	ws := &config.WorkspaceConfig{Strategy: "copy"}
	if mustHash(t, nil, ws) == mustHash(t, []string{"built"}, ws) {
		t.Error("declaring inputs did not change a put node's hash when a workspace: block is configured")
	}
}
