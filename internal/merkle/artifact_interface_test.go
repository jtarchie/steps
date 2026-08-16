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

	plain, err := GetNodeContent(&config.Config{}, config.Step{Get: "repo"}, rtype, nil, source, version)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := plain["artifact"]; ok {
		t.Errorf("unaliased get folded an artifact key: %#v", plain)
	}

	contentA, errA := GetNodeContent(&config.Config{}, config.Step{Get: "a", Resource: "repo"}, rtype, nil, source, version)
	contentB, errB := GetNodeContent(&config.Config{}, config.Step{Get: "b", Resource: "repo"}, rtype, nil, source, version)
	aliasA := hashOrFail(t, NodeKindGet, contentA, errA)
	aliasB := hashOrFail(t, NodeKindGet, contentB, errB)

	if aliasA == aliasB {
		t.Error("two aliases of the same resource hashed identically; the artifact name must distinguish them")
	}
}

// TestPutInputsAllSentinel confirms inputs: all folds a distinct sentinel
// into the hash — a view of everything-so-far and a view of nothing are
// different views.
func TestPutInputsAllSentinel(t *testing.T) {
	t.Parallel()

	rtype := config.ResourceType{Config: config.ResourceTypeConfig{Out: "true"}}

	withAll, err := PutNodeContent(&config.Config{}, config.Step{Put: "r"}, rtype, nil, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if withAll["inputs"] != "all" {
		t.Errorf(`put inputs: all folded %#v, want "all"`, withAll["inputs"])
	}
}

// TestTaskMappingValueGated confirms input_mapping/output_mapping fold into
// the hash when non-empty — they change what gets materialized where — and
// stay absent when unmapped, so an unmapped task hashes as before the field
// existed.
func TestTaskMappingValueGated(t *testing.T) {
	t.Parallel()

	base := config.ResolvedTask{Run: "true", Inputs: []string{"repo"}}
	mapped := config.ResolvedTask{
		Run: "true", Inputs: []string{"repo"},
		InputMapping: map[string]string{"repo": "source"},
	}

	taskHash := func(cfg *config.Config, rt config.ResolvedTask) string {
		content, err := TaskNodeContent(cfg, config.Step{}, rt)

		return hashOrFail(t, NodeKindTask, content, err)
	}

	if taskHash(&config.Config{}, base) == taskHash(&config.Config{}, mapped) {
		t.Error("mapping did not change the hash; it changes what gets materialized")
	}
}
