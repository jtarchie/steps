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
// records which version it built. The get is plain — no trigger:, no
// passed: — unless triggerGet adds one.
func refreshFixture(t *testing.T, check string) (*config.Config, *store.Store, string) {
	t.Helper()

	return refreshFixtureWithGet(t, check, "get: items")
}

func refreshFixtureWithGet(t *testing.T, check, getStep string) (*config.Config, *store.Store, string) {
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
  - %s
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, check, getStep, posted)

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

	// items has no trigger: and no passed:, so nothing polls it — the
	// resources page would show "never checked" forever otherwise. The run
	// that just fetched v2 is the only place that version is ever observed,
	// so it records it as the checked baseline for display.
	baseline, _, err := st.LastCheckedVersion(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v2"}` {
		t.Errorf("baseline = %s, want v2 — an unpolled resource's checked version comes from the run that resolved it", baseline)
	}
}

// TestRunDoesNotAdvanceAPolledResourcesBaseline is the counterpart: a
// resource a get step polls (trigger: true here) keeps its resource_checks
// row as the poller's dirty-bit baseline. A run recording whatever version
// IT happened to resolve would suppress a trigger for a version no poll
// ever dispatched — see internal/trigger.pollOnce and
// config.PolledResourceNames.
func TestRunDoesNotAdvanceAPolledResourcesBaseline(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")

	cfg, st, posted := refreshFixtureWithGet(t, "cat "+feed, "get: items\n    trigger: true")
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

	// The world has moved on, and this manual run sees it.
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
		t.Errorf("built %q, want v2 — the run must still see past the last poll", data)
	}

	baseline, _, err := st.LastCheckedVersion(ctx, "items")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v1"}` {
		t.Errorf("baseline = %s, want unchanged v1 — a polled resource's baseline only ever moves by a poll", baseline)
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
