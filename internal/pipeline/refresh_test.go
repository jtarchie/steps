package pipeline

// Refresh on run: a job builds against the world as of now.

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

// refreshFixture: one resource whose check reads a file, one task that
// records which version it built.
func refreshFixture(t *testing.T, check string) (*config.Config, *store.Store, string) {
	t.Helper()

	dir := t.TempDir()
	posted := filepath.Join(dir, "posted.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: %s
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: items
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, check, posted)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return cfg, st, posted
}

func runBuild(ctx context.Context, t *testing.T, cfg *config.Config, st *store.Store) error {
	t.Helper()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	return RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
}

// TestRunRefreshesResourceHistory is the requirement in one scene: whatever
// triggers a job, its resources are re-checked first, so a manual run builds
// the version that exists NOW — not the one history held from whenever a
// watcher last polled.
func TestRunRefreshesResourceHistory(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")

	cfg, st, posted := refreshFixture(t, "cat "+feed)
	ctx := context.Background()

	// What a poll recorded, some time ago: v1 was latest then.
	_, err := st.RecordVersions(ctx, "items", []map[string]any{{"n": "v1"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordCheckedVersion(ctx, "items", `{"n":"v1"}`)
	if err != nil {
		t.Fatal(err)
	}

	// The world has moved on.
	err = os.WriteFile(feed, []byte(`[{"n":"v1"},{"n":"v2"}]`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	data, err := os.ReadFile(posted) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "v2\n" {
		t.Errorf("built %q, want v2 — the run must see past the last poll", data)
	}

	// And the watcher's baseline is untouched: refresh reads the cursor,
	// only a poll advances it — a run that moved it would suppress the
	// trigger for v2 that no poll ever dispatched.
	baseline, _, err := st.LastCheckedVersion(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v1"}` {
		t.Errorf("baseline = %s, want unchanged — only a poll advances it", baseline)
	}
}

// TestRefreshFailureWarnsAndProceeds: the version record is the truth and
// checks feed it, so a check outage must not block building what is already
// known.
func TestRefreshFailureWarnsAndProceeds(t *testing.T) {
	cfg, st, posted := refreshFixture(t, `"exit 1"`)
	ctx := context.Background()

	_, err := st.RecordVersions(ctx, "items", []map[string]any{{"n": "v1"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob: %v — a failed refresh must fall back to recorded history", err)
	}

	data, err := os.ReadFile(posted) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil || string(data) != "v1\n" {
		t.Errorf("built %q (%v), want the recorded v1", data, err)
	}
}

// TestRefreshFailureWithNoHistoryStillFails: with nothing recorded there is
// nothing to fall back to, and quietly building nothing is the one outcome
// worse than failing.
func TestRefreshFailureWithNoHistoryStillFails(t *testing.T) {
	cfg, st, _ := refreshFixture(t, `"exit 1"`)

	err := runBuild(context.Background(), t, cfg, st)
	if err == nil {
		t.Fatal("RunJob succeeded with no versions from any source")
	}
}
