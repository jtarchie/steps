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

	chains, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil)
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

	chains, err := PlanChains(context.Background(), cfg, "build", steps, nil, nil)
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

	content, err := TaskNodeContent(&config.Config{}, config.Step{}, rt)
	if err != nil {
		t.Fatal(err)
	}

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

		content, err := TaskNodeContent(&config.Config{Workspace: ws}, config.Step{}, rt)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
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

		content, err := PutNodeContent(&config.Config{Workspace: ws}, config.Step{}, rt, source, nil, inputs, false)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindPut, content, "")
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

	content, err := TaskNodeContent(&config.Config{}, config.Step{}, rt)
	if err != nil {
		t.Fatal(err)
	}

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

		content, err := TaskNodeContent(&config.Config{Workspace: ws}, config.Step{}, rt)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
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

	baseContent, err := AgentContentMap(&config.Config{}, step, base)
	if err != nil {
		t.Fatal(err)
	}

	baseHash, err := HashNode(NodeKindAgent, baseContent, "")
	if err != nil {
		t.Fatal(err)
	}

	imageContent, err := AgentContentMap(&config.Config{}, step, withImage)
	if err != nil {
		t.Fatal(err)
	}

	imageHash, err := HashNode(NodeKindAgent, imageContent, "")
	if err != nil {
		t.Fatal(err)
	}

	if baseHash == imageHash {
		t.Error("setting an agent's resolved image did not change the agent node hash")
	}
}

// TestGetPutImageAffectsHash mirrors the task case for GetNodeContent and
// PutNodeContent, whose image comes from the resource type.
//
//nolint:cyclop // straight-line build/hash pairs for get and put in one test
func TestGetPutImageAffectsHash(t *testing.T) {
	t.Parallel()

	base := config.ResourceType{Config: config.ResourceTypeConfig{In: "true", Out: "true"}}
	withImage := config.ResourceType{Config: config.ResourceTypeConfig{In: "true", Out: "true"}, Image: "alpine/git"}

	source := map[string]any{"key": "v"}
	version := map[string]any{"ref": "v1"}

	baseGetContent, err := GetNodeContent(&config.Config{}, config.Step{}, base, source, version)
	if err != nil {
		t.Fatal(err)
	}

	baseGetHash, err := HashNode(NodeKindGet, baseGetContent, "")
	if err != nil {
		t.Fatal(err)
	}

	imageGetContent, err := GetNodeContent(&config.Config{}, config.Step{}, withImage, source, version)
	if err != nil {
		t.Fatal(err)
	}

	imageGetHash, err := HashNode(NodeKindGet, imageGetContent, "")
	if err != nil {
		t.Fatal(err)
	}

	if baseGetHash == imageGetHash {
		t.Error("setting a resource type's image did not change the get node hash")
	}

	basePutContent, err := PutNodeContent(&config.Config{}, config.Step{}, base, source, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	basePutHash, err := HashNode(NodeKindPut, basePutContent, "")
	if err != nil {
		t.Fatal(err)
	}

	imagePutContent, err := PutNodeContent(&config.Config{}, config.Step{}, withImage, source, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	imagePutHash, err := HashNode(NodeKindPut, imagePutContent, "")
	if err != nil {
		t.Fatal(err)
	}

	if basePutHash == imagePutHash {
		t.Error("setting a resource type's image did not change the put node hash")
	}
}

// hookCfg builds a config with one reusable task the hooks can reference.
func hookCfg() *config.Config {
	return &config.Config{
		Tasks: []config.Task{{Name: "notify", Run: "echo notified"}},
	}
}

// TestHooksOmittedFromHashWhenAbsent guards the same cache-stability guarantee
// the image tests do: a step with no hooks must hash exactly as it did before
// hooks existed — no "hooks" key in the content map.
func TestHooksOmittedFromHashWhenAbsent(t *testing.T) {
	t.Parallel()

	content, err := TaskNodeContent(hookCfg(), config.Step{Task: "build", Run: "echo hi"}, config.ResolvedTask{Name: "build", Run: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := content["hooks"]; ok {
		t.Error(`a step with no hooks should not have a "hooks" content key`)
	}

	if len(content) != 1 {
		t.Fatalf(`content = %#v, want exactly {"run": ...}`, content)
	}
}

// TestHooksAffectHash checks that adding a hook, and changing a hook, both
// change a step's hash — and that editing the tasks: entry a hook references
// (resolved hashing) changes it too.
func TestHooksAffectHash(t *testing.T) {
	t.Parallel()

	rt := config.ResolvedTask{Name: "build", Run: "echo hi"}

	mustHash := func(t *testing.T, cfg *config.Config, step config.Step) string {
		t.Helper()

		content, err := TaskNodeContent(cfg, step, rt)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	noHook := mustHash(t, hookCfg(), config.Step{Task: "build", Run: "echo hi"})

	withHook := config.Step{Task: "build", Run: "echo hi", Hooks: config.Hooks{OnFailure: &config.Step{Task: "notify"}}}
	withHookHash := mustHash(t, hookCfg(), withHook)

	if noHook == withHookHash {
		t.Error("adding an on_failure hook did not change the step hash")
	}

	// Same hook wired to on_success instead of on_failure must differ.
	movedHook := config.Step{Task: "build", Run: "echo hi", Hooks: config.Hooks{OnSuccess: &config.Step{Task: "notify"}}}
	if mustHash(t, hookCfg(), movedHook) == withHookHash {
		t.Error("moving a hook from on_failure to on_success did not change the step hash")
	}

	// Editing the referenced tasks: entry the hook resolves to must change it.
	editedCfg := &config.Config{Tasks: []config.Task{{Name: "notify", Run: "echo CHANGED"}}}
	if mustHash(t, editedCfg, withHook) == withHookHash {
		t.Error("editing the tasks: entry a hook references did not change the step hash")
	}
}

// TestAssertOmittedFromHashWhenAbsent guards cache stability: a task with no
// assert hashes exactly as before assert existed.
func TestAssertOmittedFromHashWhenAbsent(t *testing.T) {
	t.Parallel()

	content, err := TaskNodeContent(&config.Config{}, config.Step{Task: "build", Run: "echo hi"}, config.ResolvedTask{Name: "build", Run: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := content["assert"]; ok {
		t.Error(`a task with no assert should not have an "assert" content key`)
	}
}

// TestAssertAffectsHash checks that adding/changing a task assert changes its
// hash (assert alters the step's success criteria, so it must bust the cache).
func TestAssertAffectsHash(t *testing.T) {
	t.Parallel()

	code0, code1 := 0, 1

	mustHash := func(t *testing.T, assert *config.Assert) string {
		t.Helper()

		rt := config.ResolvedTask{Name: "build", Run: "echo hi", Assert: assert}

		content, err := TaskNodeContent(&config.Config{}, config.Step{Task: "build", Run: "echo hi", Assert: assert}, rt)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	none := mustHash(t, nil)
	withCode0 := mustHash(t, &config.Assert{Code: &code0})

	if none == withCode0 {
		t.Error("adding an assert did not change the task hash")
	}

	if withCode0 == mustHash(t, &config.Assert{Code: &code1}) {
		t.Error("changing assert.code did not change the task hash")
	}
}

// TestHookUnknownTaskErrors checks that a hook naming an undefined tasks:
// entry surfaces as an error at content-build (plan) time.
func TestHookUnknownTaskErrors(t *testing.T) {
	t.Parallel()

	step := config.Step{Task: "build", Run: "echo hi", Hooks: config.Hooks{Ensure: &config.Step{Task: "does-not-exist"}}}

	_, err := TaskNodeContent(&config.Config{}, step, config.ResolvedTask{Name: "build", Run: "echo hi"})
	if err == nil {
		t.Error("expected an error for a hook referencing an unknown task, got nil")
	}
}

// TestTaskNodeContentOmitsTimeout guards cache stability: a task with no
// timeout: or a different timeout: must hash identically, since timeout is
// an operational limit (like attempts), not content. This is the load-bearing
// guarantee that lets a pipeline operator add/change timeouts without
// invalidating existing cached runs.
func TestTaskNodeContentOmitsTimeout(t *testing.T) {
	t.Parallel()

	mustHash := func(t *testing.T, timeout string) string {
		t.Helper()

		rt := config.ResolvedTask{Name: "build", Run: "echo hi", Timeout: timeout}

		content, err := TaskNodeContent(&config.Config{}, config.Step{Task: "build", Run: "echo hi", Timeout: timeout}, rt)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	noTimeout := mustHash(t, "")
	shortTimeout := mustHash(t, "30s")
	longTimeout := mustHash(t, "10m")

	if noTimeout != shortTimeout {
		t.Error("adding a timeout changed the task hash (timeout should not be hashed)")
	}

	if noTimeout != longTimeout {
		t.Error("changing timeout value changed the task hash (timeout should not be hashed)")
	}

	if _, ok := func() (string, bool) {
		rt := config.ResolvedTask{Name: "build", Run: "echo hi", Timeout: "30s"}
		content, _ := TaskNodeContent(&config.Config{}, config.Step{}, rt)
		return "timeout", content["timeout"] != nil
	}(); ok {
		t.Error(`a task with timeout should not have a "timeout" content key`)
	}
}

// TestPutNodeContentOmitsTimeout mirrors TestTaskNodeContentOmitsTimeout:
// put timeouts must not affect the hash.
func TestPutNodeContentOmitsTimeout(t *testing.T) {
	t.Parallel()

	mustHash := func(t *testing.T, timeout string) string {
		t.Helper()

		rt := config.ResourceType{
			Name:   "git",
			Config: config.ResourceTypeConfig{Out: "echo out"},
		}

		content, err := PutNodeContent(&config.Config{}, config.Step{Put: "repo", Timeout: timeout}, rt, map[string]any{}, map[string]any{}, []string{}, false)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindPut, content, "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	noTimeout := mustHash(t, "")
	withTimeout := mustHash(t, "5m")

	if noTimeout != withTimeout {
		t.Error("adding a timeout changed the put hash (timeout should not be hashed)")
	}

	rt := config.ResourceType{Name: "git", Config: config.ResourceTypeConfig{Out: "echo out"}}
	content, _ := PutNodeContent(&config.Config{}, config.Step{Put: "repo", Timeout: "5m"}, rt, map[string]any{}, map[string]any{}, []string{}, false)
	if _, ok := content["timeout"]; ok {
		t.Error(`a put with timeout should not have a "timeout" content key`)
	}
}

// TestAgentContentMapOmitsTimeout mirrors the task/put timeout tests:
// agent timeouts must not affect the hash.
func TestAgentContentMapOmitsTimeout(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: []config.Agent{
			{
				Name:   "reviewer",
				Source: config.AgentSource{Model: "lmstudio/model"},
			},
		},
	}

	mustHash := func(t *testing.T, timeout string) string {
		t.Helper()

		ri := config.ResolvedInvocation{
			AgentName: "reviewer",
			ModelName: "lmstudio/model",
			Timeout:   timeout,
			MaxTurns:  8,
			Attempts:  1,
			ToolSpecs: []config.ToolSpec{},
		}

		content, err := AgentContentMap(cfg, config.Step{Agent: "reviewer", Timeout: timeout}, ri)
		if err != nil {
			t.Fatal(err)
		}

		hash, err := HashNode(NodeKindAgent, content, "")
		if err != nil {
			t.Fatal(err)
		}

		return hash
	}

	noTimeout := mustHash(t, "")
	withTimeout := mustHash(t, "10m")

	if noTimeout != withTimeout {
		t.Error("adding a timeout changed the agent hash (timeout should not be hashed)")
	}

	ri := config.ResolvedInvocation{
		AgentName: "reviewer",
		ModelName: "lmstudio/model",
		Timeout:   "10m",
		MaxTurns:  8,
		Attempts:  1,
		ToolSpecs: []config.ToolSpec{},
	}
	content, _ := AgentContentMap(cfg, config.Step{Agent: "reviewer"}, ri)
	if _, ok := content["timeout"]; ok {
		t.Error(`an agent with timeout should not have a "timeout" content key`)
	}
}
