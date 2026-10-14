package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// cachingBuild answers every lookup with hit.
type cachingBuild struct {
	workspace.BuildWorkspace

	hit bool
}

func (b cachingBuild) RestoreStep(context.Context, workspace.StepCacheRequest) (workspace.StepCacheResult, error) {
	return workspace.StepCacheResult{Hit: b.hit, Key: "key"}, nil
}

func (cachingBuild) StoreStep(context.Context, string, workspace.StepCacheRequest) error { return nil }

// TestReuseStepRecordsOnlyAHit: a miss means the step is about to run, and
// recording it succeeded first shows it green for its whole run — and leaves
// it green if the process dies mid-conversation. A hit is the work done, by an
// earlier run, and is recorded as such.
func TestReuseStepRecordsOnlyAHit(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "writer", Outputs: []string{"report"}}

	for name, hit := range map[string]bool{"miss": false, "hit": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}

			t.Cleanup(func() { _ = st.Close() })

			r := stepRunner{cfg: &config.Config{}, jobName: "build", bw: cachingBuild{hit: hit}, st: st}
			node := merkle.Node{Hash: "agent-node", Kind: merkle.NodeKindAgent, Resource: "writer"}

			cached, err := reuseStep(t.Context(), r, step, workspace.StepCacheRequest{}, node)
			if err != nil || cached.Hit != hit {
				t.Fatalf("reuseStep = %+v, %v; want Hit=%v", cached, err, hit)
			}

			succeeded, err := st.HasNodeSucceeded(t.Context(), "build", node.Hash)
			if err != nil {
				t.Fatalf("HasNodeSucceeded: %v", err)
			}

			if succeeded != hit {
				t.Errorf("node recorded succeeded = %v after a %s; want %v", succeeded, name, hit)
			}
		})
	}
}
