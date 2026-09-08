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
	baseline, _, err := lastCheckedVersion(t, st, "items")
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

	baseline, _, err := lastCheckedVersion(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v1"}` {
		t.Errorf("baseline = %s, want unchanged v1 — a polled resource's baseline only ever moves by a poll", baseline)
	}
}

// TestRunDoesNotRecordResolvedVersionOnFetchFailure: a version that fails to
// fetch must not appear as the resource's "checked" version on the web UI's
// resources page — that page would then show a version nothing actually
// retrieved, indistinguishable from a real, successful check. This is what
// separates recordResolvedVersion's write from recordFetchedVersion's: the
// latter's effect (recordPassedVersions) is already gated on the whole build
// succeeding, and recordResolvedVersion must be gated on the FETCH
// succeeding for the same reason.
func TestRunDoesNotRecordResolvedVersionOnFetchFailure(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: cat %s
    in: exit 1
resources:
- name: items
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: items
`, feed)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(feed, []byte(`[{"n":"v1"}]`), 0o600)
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

	ctx := context.Background()

	err = runBuild(ctx, t, cfg, st)
	if err == nil {
		t.Fatal("RunJob succeeded despite in: exiting 1 — the fetch should have failed")
	}

	_, found, err := lastCheckedVersion(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if found {
		t.Error("resource_checks has an entry for items even though its fetch failed — a failed fetch must not be shown as the resource's checked version")
	}
}

// TestRunRecordsResolvedVersionForInPlaceGet exercises the OTHER call site of
// recordResolvedVersion: a job's second get, which — unlike its first —
// fetches in place inside the first get's triggered build
// (fetchGetStepInPlace) rather than fanning out its own
// (runTriggeredBuild, already covered by TestRunRefreshesResourceHistory).
// Both call the same recordResolvedVersion; only a two-get job proves the
// in-place one actually runs.
func TestRunRecordsResolvedVersionForInPlaceGet(t *testing.T) {
	dir := t.TempDir()
	feed1 := filepath.Join(dir, "items.json")
	feed2 := filepath.Join(dir, "extra.json")
	posted := filepath.Join(dir, "posted.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: listing1
  config:
    check: cat %s
    in: echo {{ .version.n | shellquote }} > n.txt
- name: listing2
  config:
    check: cat %s
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: listing1
  source: {}
- name: extra
  type: listing2
  source: {}
jobs:
- name: build
  plan:
  - get: items
  - get: extra
  - task: work
    inputs: [items, extra]
    run: cat items/n.txt extra/n.txt >> %s
`, feed1, feed2, posted)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(feed1, []byte(`[{"n":"i1"}]`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(feed2, []byte(`[{"n":"e1"}]`), 0o600)
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

	ctx := context.Background()

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	// "extra" is the job's SECOND get — fetched via fetchGetStepInPlace
	// inside "items"'s triggered build, not via runTriggeredBuild's own call.
	baseline, _, err := lastCheckedVersion(t, st, "extra")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"e1"}` {
		t.Errorf("baseline = %s, want e1 — the in-place get path records a resolved version too", baseline)
	}
}

// TestRunSkipsResolvedVersionForPassedOnlyResource proves recordResolvedVersion
// stays out of resource_checks for a resource reached only via passed: (no
// trigger: anywhere in the pipeline) — config.Config.ResourceIsPolled names
// it, per PolledResourceNames' doc, as territory the poller owns. A run
// recording its own resolved version there would corrupt the poller's
// dirty-bit baseline for a resource nothing has ever polled yet.
func TestRunSkipsResolvedVersionForPassedOnlyResource(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")
	postedBuild := filepath.Join(dir, "build.txt")
	postedDeploy := filepath.Join(dir, "deploy.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: cat %s
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
- name: deploy
  plan:
  - get: items
    passed: [build]
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, feed, postedBuild, postedDeploy)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(feed, []byte(`[{"n":"v1"}]`), 0o600)
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

	ctx := context.Background()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob(build): %v", err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[1], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob(deploy): %v", err)
	}

	// items is referenced only via passed: (no trigger: anywhere), so
	// ResourceIsPolled names it — recordResolvedVersion must skip it in both
	// jobs, leaving resource_checks untouched.
	_, found, err := lastCheckedVersion(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if found {
		t.Error("resource_checks has an entry for items — a passed:-only resource must be left to the poller, never a run")
	}
}

// TestRunRefreshesResolvedVersionOnFanOutCacheSkip proves the OTHER skip path
// calls recordResolvedVersion too: a job's FIRST get always fetches through
// fanOutGet (see runTriggeredBuild's own comment on why), so once its chain
// is cached, fanOutGet's cache-skip branch — not fetchGetStepInPlace's — is
// the one a second, unchanged run takes. Without the call there, an unpolled
// resource's resource_checks row goes stale the moment its job starts being
// skipped, even though the job keeps succeeding on the same version.
func TestRunRefreshesResolvedVersionOnFanOutCacheSkip(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")

	cfg, st, posted := refreshFixture(t, "cat "+feed)
	ctx := context.Background()

	err := os.WriteFile(feed, []byte(`[{"n":"v1"}]`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob (1st): %v", err)
	}

	// Corrupt what the first run recorded, so the second run — which must
	// hit the cache-skip branch, since nothing about the pipeline or feed
	// changed — is the only thing that can repair it.
	err = st.RecordCheckedVersion(ctx, "items", `{"n":"stale"}`)
	if err != nil {
		t.Fatal(err)
	}

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob (2nd): %v", err)
	}

	data, err := os.ReadFile(posted) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "v1\n" {
		t.Fatalf("built %q, want a single v1 — the second run's get (and so its task) is cache-skipped", data)
	}

	baseline, _, err := lastCheckedVersion(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v1"}` {
		t.Errorf("baseline = %s, want v1 — a cache-skipped fan-out get must still refresh an unpolled resource's checked version", baseline)
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

// TestRunDoesNotRegressResolvedVersionOnAPinnedRun: a --pin'd run resolves
// whatever OLDER version the pin names, on purpose — but recordResolvedVersion
// must not mistake that for a fresh "latest" and drag an unpolled resource's
// displayed checked version backward. A regression here would also feed
// checkCursorFor (refresh.go) a stale cursor on the run after, making it
// re-walk ground an earlier, unpinned run already covered.
func TestRunDoesNotRegressResolvedVersionOnAPinnedRun(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")

	cfg, st, _ := refreshFixture(t, "cat "+feed)
	ctx := context.Background()

	// First, ordinary run: only v1 exists, so it's both fetched and recorded.
	err := os.WriteFile(feed, []byte(`[{"n":"v1"}]`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob (v1): %v", err)
	}

	// The world moves on and a second ordinary run sees it, advancing the
	// displayed checked version to v2 — TestRunRefreshesResourceHistory
	// already proves this half.
	err = os.WriteFile(feed, []byte(`[{"n":"v1"},{"n":"v2"}]`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = runBuild(ctx, t, cfg, st)
	if err != nil {
		t.Fatalf("RunJob (v2): %v", err)
	}

	// A one-off, explicitly pinned rerun against the OLDER v1 — an operator
	// re-running a stale build, say. It must fetch v1 (the pin wins), but
	// must NOT be mistaken for a new "latest".
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, &cfg.Jobs[0], map[string]string{"n": "v1"}, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob (pinned v1): %v", err)
	}

	baseline, _, err := lastCheckedVersion(t, st, "items")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v2"}` {
		t.Errorf("baseline = %s, want unchanged v2 — a pinned run's older fetch must not regress the displayed checked version", baseline)
	}
}

// lastCheckedVersion is the check cursor's JSON, which is all these tests
// compare — the row it sits on carries a timestamp they have nothing to say
// about.
func lastCheckedVersion(t *testing.T, st *store.Store, name string) (string, bool, error) {
	t.Helper()

	last, found, err := st.LastChecked(context.Background(), name)
	if err != nil {
		return "", false, fmt.Errorf("reading the check cursor for %q: %w", name, err)
	}

	return last.Version, found, nil
}
