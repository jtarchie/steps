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

// TestImageOmittedFromHashWhenEmpty guards the same cache-stability
// guarantee as the inputs/outputs tests above, but for image: — unlike
// inputs/outputs, image is gated on its own value rather than on ws != nil
// (see TaskNodeContent's doc comment), so this checks the no-workspace case
// specifically: a pipeline that never sets image: must hash exactly as it
// did before this field existed.
func TestImageOmittedFromHashWhenEmpty(t *testing.T) {
	t.Parallel()

	rt := config.ResolvedTask{Name: "build", Run: "echo hi"}

	content := TaskNodeContent(rt, nil)
	if len(content) != 1 {
		t.Fatalf(`TaskNodeContent(rt, nil) = %#v, want exactly {"run": ...}, no image key`, content)
	}

	if _, ok := content["image"]; ok {
		t.Error(`TaskNodeContent with no image set should not have an "image" key`)
	}
}

// TestTaskImageAffectsHashRegardlessOfWorkspace checks that (unlike
// inputs/outputs) a task's image changes its hash whether or not a
// workspace: block is configured, since image alters what the run: command
// actually executes against no matter which workspace mode is active.
func TestTaskImageAffectsHashRegardlessOfWorkspace(t *testing.T) {
	t.Parallel()

	base := config.ResolvedTask{Name: "build", Run: "echo hi"}
	withImage := config.ResolvedTask{Name: "build", Run: "echo hi", Image: "alpine"}
	withOtherImage := config.ResolvedTask{Name: "build", Run: "echo hi", Image: "golang:1.26"}

	mustHash := func(t *testing.T, rt config.ResolvedTask, ws *config.WorkspaceConfig) string {
		t.Helper()

		hash, err := HashNode(NodeKindTask, TaskNodeContent(rt, ws), "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	for _, ws := range []*config.WorkspaceConfig{nil, {Strategy: "copy"}} {
		if mustHash(t, base, ws) == mustHash(t, withImage, ws) {
			t.Errorf("setting image did not change the task hash (ws=%v)", ws)
		}

		if mustHash(t, withImage, ws) == mustHash(t, withOtherImage, ws) {
			t.Errorf("two different images produced the same task hash (ws=%v)", ws)
		}
	}
}

// TestAgentImageAffectsHash mirrors the task case for AgentContentMap.
func TestAgentImageAffectsHash(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer"}
	base := config.ResolvedInvocation{AgentName: "reviewer"}
	withImage := config.ResolvedInvocation{AgentName: "reviewer", Image: "python:3.12"}

	baseHash, err := HashNode(NodeKindAgent, AgentContentMap(step, base, nil), "")
	if err != nil {
		t.Fatal(err)
	}

	imageHash, err := HashNode(NodeKindAgent, AgentContentMap(step, withImage, nil), "")
	if err != nil {
		t.Fatal(err)
	}

	if baseHash == imageHash {
		t.Error("setting an agent's resolved image did not change the agent node hash")
	}
}

// TestGetPutImageAffectsHash mirrors the task case for GetNodeContent and
// PutNodeContent, whose image comes from the resource type.
func TestGetPutImageAffectsHash(t *testing.T) {
	t.Parallel()

	base := config.ResourceType{Config: config.ResourceTypeConfig{In: "true", Out: "true"}}
	withImage := config.ResourceType{Config: config.ResourceTypeConfig{In: "true", Out: "true"}, Image: "alpine/git"}

	source := map[string]any{"key": "v"}
	version := map[string]any{"ref": "v1"}

	baseGetHash, err := HashNode(NodeKindGet, GetNodeContent(base, source, version), "")
	if err != nil {
		t.Fatal(err)
	}

	imageGetHash, err := HashNode(NodeKindGet, GetNodeContent(withImage, source, version), "")
	if err != nil {
		t.Fatal(err)
	}

	if baseGetHash == imageGetHash {
		t.Error("setting a resource type's image did not change the get node hash")
	}

	basePutHash, err := HashNode(NodeKindPut, PutNodeContent(base, source, nil, nil, nil), "")
	if err != nil {
		t.Fatal(err)
	}

	imagePutHash, err := HashNode(NodeKindPut, PutNodeContent(withImage, source, nil, nil, nil), "")
	if err != nil {
		t.Fatal(err)
	}

	if basePutHash == imagePutHash {
		t.Error("setting a resource type's image did not change the put node hash")
	}
}
