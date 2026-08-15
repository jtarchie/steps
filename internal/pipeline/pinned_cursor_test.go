package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestConformancePinnedRunConsumesNothing: naming a version is an
// instruction outside the every-flow, so a pinned run neither reads the
// consumed cursor (Cache.unconsumed, long-standing) nor writes it (this
// test's reason to exist).
//
// The write half became load-bearing when the cursor became a high-water
// mark. A pin outside history is minted at the TOP discovery order, and
// taking it would leap the mark over every unbuilt version below — here, a
// v0 hotfix pin silently cancelling the owed v4 and v5. The set-based cursor
// recorded pins harmlessly; a mark cannot.
//
// Concourse's parallel: a pinned resource is excluded from version-selection
// entirely, and building a pinned version never advances NextEveryVersion's
// cursor, because discovery order is never re-minted by a build.
func TestConformancePinnedRunConsumesNothing(t *testing.T) {
	dir := t.TempDir()
	posted := filepath.Join(dir, "posted.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: printf '[{"n":"v0"},{"n":"v1"},{"n":"v2"},{"n":"v3"},{"n":"v4"},{"n":"v5"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: items
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, posted)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	// A check filed v1..v3; the job built them (mark = 3).
	versions := []map[string]any{{"n": "v1"}, {"n": "v2"}, {"n": "v3"}}
	_, err = st.RecordVersions(ctx, "items", versions, 0)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	assertConformanceLineCount(t, posted, 3)

	// v4, v5 arrive in history — owed work.
	more := []map[string]any{{"n": "v1"}, {"n": "v2"}, {"n": "v3"}, {"n": "v4"}, {"n": "v5"}}
	_, err = st.RecordVersions(ctx, "items", more, 0)
	if err != nil {
		t.Fatal(err)
	}

	// An operator pins v0 — a hotfix version history never held.
	err = RunJob(ctx, cfg, &cfg.Jobs[0], map[string]string{"n": "v0"}, provider, st, false)
	if err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	assertConformanceLineCount(t, posted, 4)

	// A normal run must still build the owed v4 and v5.
	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("final run: %v", err)
	}

	data, _ := os.ReadFile(posted) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	t.Logf("posted:\n%s", data)
	assertConformanceLineCount(t, posted, 6)
}
