package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func hashOrFail(t *testing.T, kind NodeKind, content map[string]any, err error) string {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}

	hash, err := HashNode(kind, content, "")
	if err != nil {
		t.Fatal(err)
	}

	return hash
}

// TestGetNodeContentAliasValueGated confirms an unaliased get hashes exactly as
// before this feature (no "artifact" key), while a get: aliasing its resource
// folds the artifact name so two aliases of the same resource hash distinctly.
func TestGetNodeContentAliasValueGated(t *testing.T) {
	t.Parallel()

	rtype := config.ResourceType{Config: config.ResourceTypeConfig{In: "true"}}
	source := map[string]any{"repo": "x"}
	version := map[string]any{"ref": "v1"}

	plain, err := GetNodeContent(&config.Config{}, config.Step{Get: "repo"}, rtype, source, version)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := plain["artifact"]; ok {
		t.Errorf("unaliased get folded an artifact key: %#v", plain)
	}

	contentA, errA := GetNodeContent(&config.Config{}, config.Step{Get: "a", Resource: "repo"}, rtype, source, version)
	contentB, errB := GetNodeContent(&config.Config{}, config.Step{Get: "b", Resource: "repo"}, rtype, source, version)
	aliasA := hashOrFail(t, NodeKindGet, contentA, errA)
	aliasB := hashOrFail(t, NodeKindGet, contentB, errB)

	if aliasA == aliasB {
		t.Error("two aliases of the same resource hashed identically; the artifact name must distinguish them")
	}
}

// TestPutInputsAllValueGated confirms inputs: all folds a distinct sentinel,
// but only under a workspace: block (declarations are inert to the hash in
// shared mode).
func TestPutInputsAllValueGated(t *testing.T) {
	t.Parallel()

	rtype := config.ResourceType{Config: config.ResourceTypeConfig{Out: "true"}}
	ws := &config.WorkspaceConfig{Strategy: "copy"}

	shared, err := PutNodeContent(&config.Config{}, config.Step{Put: "r"}, rtype, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := shared["inputs"]; ok {
		t.Errorf("shared-mode put folded inputs into the hash: %#v", shared)
	}

	withAll, err := PutNodeContent(&config.Config{Workspace: ws}, config.Step{Put: "r"}, rtype, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if withAll["inputs"] != "all" {
		t.Errorf(`workspace-mode put inputs: all folded %#v, want "all"`, withAll["inputs"])
	}
}

// TestTaskMappingValueGated confirms input_mapping/output_mapping fold into the
// hash only under a workspace: block and only when non-empty.
func TestTaskMappingValueGated(t *testing.T) {
	t.Parallel()

	ws := &config.WorkspaceConfig{Strategy: "copy"}
	base := config.ResolvedTask{Run: "true", Inputs: []string{"repo"}}
	mapped := config.ResolvedTask{
		Run: "true", Inputs: []string{"repo"},
		InputMapping: map[string]string{"repo": "source"},
	}

	taskHash := func(cfg *config.Config, rt config.ResolvedTask) string {
		content, err := TaskNodeContent(cfg, config.Step{}, rt)

		return hashOrFail(t, NodeKindTask, content, err)
	}

	// Shared mode: mapping is inert.
	if taskHash(&config.Config{}, base) != taskHash(&config.Config{}, mapped) {
		t.Error("mapping changed the shared-mode hash; declarations must be inert without workspace:")
	}

	// Workspace mode: mapping is load-bearing.
	wsBase := taskHash(&config.Config{Workspace: ws}, base)
	wsMapped := taskHash(&config.Config{Workspace: ws}, mapped)

	if wsBase == wsMapped {
		t.Error("mapping did not change the workspace-mode hash; it changes what gets materialized")
	}
}
