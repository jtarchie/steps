package trigger

// A passed:-constrained job builds the version that went green upstream, not
// whatever is newest by the time it runs.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestPassedFanOutOnlyBuildsWhatPassed covers a way the gate could fail open.
//
// `passed:` is checked at poll time against ONE version — the newest, and the
// check is "did that exact version go green upstream". A get that also says
// `version: every` then fans out over every version the resource reports,
// which includes older ones nothing has proved anything about. Handing the
// job the resource's whole list would deploy them; handing it the version the
// gate actually cleared does not.
//
// The older version here is not hypothetical: any resource that reports more
// than one version per check has them, and a cold start has a whole backlog.
func TestPassedFanOutOnlyBuildsWhatPassed(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"r0"}]`)

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: listing
  config:
    check: cat `+versions+`
    in: echo {{ .version.ref | shellquote }} > ref.txt
resources:
- name: repo
  type: listing
  source: {}
jobs:
- name: test
  plan:
  - get: repo
    trigger: true
- name: deploy
  plan:
  - get: repo
    trigger: true
    version: every
    passed: [test]
  - task: ship
    inputs: [repo]
    run: cat repo/ref.txt >> `+deployed+`
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// Cold start seeds the baseline on r0 and enqueues nothing.
	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// test goes green on r1 and only r1. r0 never passed anything.
	encoded, err := json.Marshal(map[string]any{"ref": "r1"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "build-1")
	if err != nil {
		t.Fatal(err)
	}

	writeVersions(t, versions, `[{"ref":"r0"},{"ref":"r1"}]`)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy never ran, so the gate proves nothing: %v", err)
	}

	got := strings.Fields(string(data))
	if len(got) != 1 || got[0] != "r1" {
		t.Errorf("deployed %v, want only [r1] — r0 never passed test", got)
	}
}

// TestPassedSurvivesAQuietCursorCheck covers the interaction that made every
// `passed:` gate stop working once checks became cursor-driven.
//
// A `passed:` constraint is evaluated against a resource's CURRENT version on
// every poll — necessarily, because the poll that first sees a version comes
// before the upstream job goes green on it, so the gate has to be re-asked
// later. That re-asking needs the resource to be in the poll's observations.
//
// A cursor-driven check asks only for what it has not seen, so it answers
// with nothing almost every poll. Treating "reported nothing" as "has no
// version" dropped the resource out of the observations entirely, and the
// downstream job was then held back forever — silently, since nothing
// distinguishes "waiting on upstream" from "never asked".
//
// Before the cursor a check re-reported its whole window every time, so this
// could not arise; it took a cursor-driven pipeline to expose it.
func TestPassedSurvivesAQuietCursorCheck(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	deployed := filepath.Join(dir, "deployed.txt")

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' `+feed+`
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: repo
  type: feed
  source: {}
jobs:
- name: test
  plan:
  - get: repo
    trigger: true
- name: deploy
  plan:
  - get: repo
    trigger: true
    passed: [test]
  - task: ship
    inputs: [repo]
    run: cat repo/n.txt >> `+deployed+`
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	writeVersions(t, feed, "1\n")

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// Item 2 arrives and test is triggered for it.
	writeVersions(t, feed, "1\n2\n")

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// test goes green on item 2, which is what deploy is waiting for.
	encoded, err := json.Marshal(map[string]any{"n": "2"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "build-1")
	if err != nil {
		t.Fatal(err)
	}

	// Quiet polls: the check has nothing new to report, which is where the
	// gate used to stop being asked at all.
	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (quiet): %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy was never released for the version test passed: %v", err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "2" {
		t.Errorf("deployed %v, want [2]", got)
	}
}

// passedFixture builds the two-job shape every passed: test uses — test gets
// repo and deploy gets repo gated on test — with deploy's task recording what
// it actually built.
// testRun is the test job's task command, so a scenario can make specific
// versions FAIL upstream — a test job with no failing task goes green on
// whatever it fetches, which quietly rewrites "nothing tested v6" into "v6
// passed".
func passedFixture(t *testing.T, dir, versions, deployed, testRun string) *config.Config {
	t.Helper()

	return loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: listing
  config:
    check: cat `+versions+`
    in: echo {{ .version.ref | shellquote }} > ref.txt
resources:
- name: repo
  type: listing
  source: {}
jobs:
- name: test
  plan:
  - get: repo
    trigger: true
  - task: check
    inputs: [repo]
    run: `+testRun+`
- name: deploy
  plan:
  - get: repo
    trigger: true
    passed: [test]
  - task: ship
    inputs: [repo]
    run: cat repo/ref.txt >> `+deployed+`
`)
}

// drainAll drains the queue, tolerating failed jobs — scenarios here make
// upstream jobs fail on purpose.
func drainAll(ctx context.Context, t *testing.T, cfg *config.Config, st *store.Store) {
	t.Helper()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	for range 10 {
		ran, err := drainOne(ctx, cfg, provider, st, nil, false)
		if err != nil {
			continue // a failing upstream job is part of the scenario
		}

		if !ran {
			return
		}
	}

	t.Fatal("queue did not drain")
}

func recordGreen(t *testing.T, st *store.Store, jobName, ref, buildID string) {
	t.Helper()

	encoded, err := json.Marshal(map[string]any{"ref": ref})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(context.Background(), jobName, "repo", string(encoded), buildID)
	if err != nil {
		t.Fatal(err)
	}
}

// TestPassedBuildsTheVersionThatPassedNotTheNewest closes the gap between
// enqueue and claim. The gate used to be checked only when the job was
// QUEUED; the build then resolved plain latest-of-history at claim time, so a
// version that arrived in between — which nothing had tested — was what
// actually shipped, and its success was then recorded green for anything
// further downstream. The constraint is now part of resolution itself:
// whatever is newest AND green when the build runs is what it builds.
// Concourse pins a build's inputs to the validated set for the same reason.
func TestPassedBuildsTheVersionThatPassedNotTheNewest(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"v1"}]`)

	cfg := passedFixture(t, dir, versions, deployed, `test "$(cat repo/ref.txt)" != v3`)
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// v2 arrives and test goes green on it; the poll queues deploy.
	writeVersions(t, versions, `[{"ref":"v1"},{"ref":"v2"}]`)
	recordGreen(t, st, "test", "v2", "b1")

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// v3 lands BEFORE any worker claims the queued row. Nothing has tested it.
	writeVersions(t, versions, `[{"ref":"v1"},{"ref":"v2"},{"ref":"v3"}]`)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (v3): %v", err)
	}

	drainAll(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy never ran: %v", err)
	}

	for _, ref := range strings.Fields(string(data)) {
		if ref == "v3" {
			t.Errorf("deployed %v — v3 was built under a passed: gate nothing tested it against", strings.Fields(string(data)))
		}
	}
}

// TestManualRunHonorsPassed: the gate binds wherever resolution happens, so
// `steps run deploy` cannot ship what the watcher would have held back.
// Before, nothing outside the watch loop consulted passed: at all.
func TestManualRunHonorsPassed(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"v1"},{"ref":"v2"}]`)

	cfg := passedFixture(t, dir, versions, deployed, "true")
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// History holds v1 and v2; only v1 is green.
	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	recordGreen(t, st, "test", "v1", "b1")

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	deploy := &cfg.Jobs[1]

	err = pipeline.RunJob(ctx, cfg, deploy, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "v1" {
		t.Errorf("manual run deployed %v, want [v1] — the newest green version, not the newest version", got)
	}

	// And with the green record gone entirely, the gate holds: the get fails
	// rather than resolving around the constraint.
	fresh := mustOpenStore(t, t.TempDir())

	_, err = pollOnce(ctx, cfg, fresh)
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.RunJob(ctx, cfg, deploy, nil, provider, fresh, false)
	if err == nil {
		t.Error("a manual run with nothing green succeeded; passed: was bypassed")
	}
}

// TestPassedReleasesDespiteAFailingHead is the starvation half. The candidate
// used to be only the currently-observed HEAD version, so when the head kept
// failing upstream the question every poll asked was "did test pass the
// head?" — no, forever — while a validated older version sat in history.
// Concourse selects the latest version satisfying the constraint; so does
// this now.
func TestPassedReleasesDespiteAFailingHead(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"v5"}]`)

	cfg := passedFixture(t, dir, versions, deployed, `test "$(cat repo/ref.txt)" != v6`)
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// test goes green on v5. Then v6 arrives — and keeps failing test, so it
	// never goes green.
	recordGreen(t, st, "test", "v5", "b1")
	writeVersions(t, versions, `[{"ref":"v5"},{"ref":"v6"}]`)

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if !contains(enqueued, "deploy") {
		t.Fatalf("enqueued = %v — deploy starved because the head is failing, though v5 passed", enqueued)
	}

	drainAll(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy never ran: %v", err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "v5" {
		t.Errorf("deployed %v, want [v5] — the version that actually passed", got)
	}
}

// TestFanOutJobsRecordGreenVersions covers the finding the doc corpus caught
// that a review of the diff did not: fetched versions were registered only on
// the in-place get path, and a job whose FIRST get is trigger-eligible
// fetches through the fan-out path — so `unit` could go green forever without
// one job_versions row, and a passed: [unit] gate downstream could never
// open. Latent while the gate was judged at trigger time against rows tests
// planted by hand; loud the moment resolution read the table for real.
func TestFanOutJobsRecordGreenVersions(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"v1"}]`)

	cfg := passedFixture(t, dir, versions, deployed, "true")
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// unit runs and goes green — through the same path a triggered run
	// takes, with nothing recorded by hand.
	err = pipeline.RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("unit: %v", err)
	}

	// That alone must be enough for the gate to open.
	err = pipeline.RunJob(ctx, cfg, &cfg.Jobs[1], nil, provider, st, false)
	if err != nil {
		t.Fatalf("deploy: %v — unit's green run left no record the gate could see", err)
	}

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "v1" {
		t.Errorf("deployed %v, want [v1]", got)
	}
}

// TestPassedReleasesAJobOnce is the fact that stops a gate becoming a loop.
//
// A `passed:` constraint is re-evaluated on EVERY poll — it has to be, since
// the poll that first sees a version comes before the upstream job goes green
// on it. So the release has to be idempotent: once the downstream job has
// itself succeeded against the green version, later polls must find nothing
// to release. Without that, a gate that opens once opens forever, and the
// thing it gates is usually a deploy.
//
// It went untested, which mutation testing is what found: the check for
// "already ran these" could be inverted and every existing passed: test still
// passed, because they all stop after the first poll that releases.
func TestPassedReleasesAJobOnce(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"r0"}]`)

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: listing
  config:
    check: cat `+versions+`
    in: echo {{ .version.ref | shellquote }} > ref.txt
resources:
- name: repo
  type: listing
  source: {}
jobs:
- name: test
  plan:
  - get: repo
    trigger: true
- name: deploy
  plan:
  - get: repo
    trigger: true
    passed: [test]
  - task: ship
    inputs: [repo]
    run: cat repo/ref.txt >> `+deployed+`
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	encoded, err := json.Marshal(map[string]any{"ref": "r1"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "build-1")
	if err != nil {
		t.Fatal(err)
	}

	writeVersions(t, versions, `[{"ref":"r0"},{"ref":"r1"}]`)

	// The poll that opens the gate, and the build it releases.
	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (release): %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy never ran, so the gate proves nothing: %v", err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "r1" {
		t.Errorf("deployed %v, want r1", got)
	}

	// Two more polls with nothing new upstream. The constraint is re-checked
	// each time — that is by design — and must now find the job has already
	// run against the version it would release.
	for attempt := 1; attempt <= 2; attempt++ {
		assertQuietPollReleasesNothing(ctx, t, cfg, st, attempt)
	}
}

// assertQuietPollReleasesNothing polls once and requires that no gated job
// came back out of the gate.
//
// Asserted on what the POLL enqueued, not on what the build wrote: a
// re-released job runs the same content, hits the merkle cache and produces
// no second line, so a side effect cannot tell a gate that fired once from
// one firing every 30 seconds forever.
func assertQuietPollReleasesNothing(
	ctx context.Context, t *testing.T, cfg *config.Config, st *store.Store, attempt int,
) {
	t.Helper()

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (quiet %d): %v", attempt, err)
	}

	for _, job := range enqueued {
		if job == "deploy" {
			t.Fatalf("poll %d released deploy again; a gate that opens once must not reopen", attempt)
		}
	}

	drainQueue(ctx, t, cfg, st)
}
