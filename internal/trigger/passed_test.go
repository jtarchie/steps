package trigger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
)

// passedPipeline builds the shape the story describes: one job tests a
// resource, another deploys it but only against versions that test passed.
func passedPipeline(t *testing.T, versionsPath string) *config.Config {
	t.Helper()

	return &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name: "listing",
			Config: config.ResourceTypeConfig{
				Check: "cat " + versionsPath,
				In:    "echo fetched",
			},
		}},
		Resources: []config.Resource{{Name: "repo", Type: "listing"}},
		Jobs: []config.Job{
			{
				Name: "test",
				Plan: []config.Step{
					{Get: "repo", Trigger: true},
					{Task: "run-tests", Run: "true", Inputs: config.Inputs()},
				},
			},
			{
				Name: "deploy",
				Plan: []config.Step{
					{Get: "repo", Trigger: true, Passed: []string{"test"}},
					{Task: "ship", Run: "true", Inputs: config.Inputs()},
				},
			},
		},
	}
}

// TestPassedHoldsBackUntilUpstreamIsGreen is the correctness gap the feature
// closes: without it, watch triggers `deploy` on a commit `test` already
// failed on, and there is no way to say otherwise.
func TestPassedHoldsBackUntilUpstreamIsGreen(t *testing.T) {
	dir := t.TempDir()
	versions := dir + "/versions.json"
	writeVersions(t, versions, `[{"ref":"v1"}]`)

	cfg := passedPipeline(t, versions)
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// Seed the baseline so the next poll sees a change rather than a first
	// sighting.
	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	writeVersions(t, versions, `[{"ref":"v2"}]`)

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// test is triggered; deploy is held back, because no version has passed
	// test yet.
	if !contains(enqueued, "test") {
		t.Errorf("enqueued = %v, want the unconstrained job to trigger", enqueued)
	}

	if contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy held back until a version passes test", enqueued)
	}
}

// TestPassedReleasesOnceUpstreamHasPassedThatVersion is the other half: the
// constraint is a gate, not a block. Once the upstream job is green on the
// exact version, the downstream job runs.
func TestPassedReleasesOnceUpstreamHasPassedThatVersion(t *testing.T) {
	dir := t.TempDir()
	versions := dir + "/versions.json"
	writeVersions(t, versions, `[{"ref":"v1"}]`)

	cfg := passedPipeline(t, versions)
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// test goes green against v2.
	writeVersions(t, versions, `[{"ref":"v2"}]`)

	encoded, err := json.Marshal(map[string]any{"ref": "v2"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "b1")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if !contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy released now that v2 passed test", enqueued)
	}
}

// passedAllUpstream mirrors what jobReadyFor does now: ask each named upstream
// job whether it has one build green against the whole constrained set. The
// per-resource question this replaced could not express the set at all.
func passedAllUpstream(t *testing.T, st *store.Store, upstream []string, resource string, version map[string]any) bool {
	t.Helper()

	for _, jobName := range upstream {
		passed, err := pipeline.VersionSetPassedUpstream(context.Background(), st, jobName,
			map[string]map[string]any{resource: version})
		if err != nil {
			t.Fatalf("VersionSetPassedUpstream(%q): %v", jobName, err)
		}

		if !passed {
			return false
		}
	}

	return true
}

// TestVersionPassedUpstreamRequiresEveryJob verifies passed: [a, b] means
// BOTH, not either — a version green in one place and red in another must not
// satisfy it.
func TestVersionPassedUpstreamRequiresEveryJob(t *testing.T) {
	dir := t.TempDir()
	st := mustOpenStore(t, dir)
	ctx := context.Background()
	version := map[string]any{"ref": "v1"}

	encoded, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "unit", "repo", string(encoded), "b1")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed := passedAllUpstream(t, st, []string{"unit", "lint"}, "repo", version)

	if passed {
		t.Error("a version green in one of two required jobs satisfied the constraint")
	}

	err = st.RecordPassedVersion(ctx, "lint", "repo", string(encoded), "b1")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed = passedAllUpstream(t, st, []string{"unit", "lint"}, "repo", version)

	if !passed {
		t.Error("a version green in both required jobs did not satisfy the constraint")
	}
}

// TestPassedIsPerVersion verifies the constraint is about THIS version, not
// about the job having ever been green. A job green on v1 must not release v2.
func TestPassedIsPerVersion(t *testing.T) {
	dir := t.TempDir()
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	encoded, err := json.Marshal(map[string]any{"ref": "v1"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "b1")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed := passedAllUpstream(t, st, []string{"test"}, "repo", map[string]any{"ref": "v2"})

	if passed {
		t.Error("a job green on v1 released v2; passed: is per version, not per job")
	}
}

func contains(names []string, want string) bool {
	return strings.Contains(strings.Join(names, ","), want)
}

// TestPassedReleasesOnALaterPoll is the test the original one should have
// been. It never records the upstream pass by hand BEFORE the poll that first
// sees the version — which is exactly what hid the defect: the recorded
// version advanced past the held-back job, the resource stopped being dirty,
// and `deploy` was never enqueued for that version again. By induction it
// never ran for any version, because an upstream job can only go green AFTER
// the poll that observed the change.
func TestPassedReleasesOnALaterPoll(t *testing.T) {
	dir := t.TempDir()
	versions := dir + "/versions.json"

	writeVersions(t, versions, `[{"ref":"v1"}]`)

	cfg := passedPipeline(t, versions)
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// Baseline, so the next poll sees a change rather than a first sighting.
	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	writeVersions(t, versions, `[{"ref":"v2"}]`)

	// Poll 1: test triggers, deploy is held back.
	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if contains(enqueued, "deploy") {
		t.Fatalf("enqueued = %v, want deploy held back before test has passed v2", enqueued)
	}

	// test now runs and goes green on v2 — the ordinary sequence, not a
	// hand-placed row before the fact.
	encoded, err := json.Marshal(map[string]any{"ref": "v2"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "b1")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	// Poll 2: the version has not changed again, and it must not need to.
	enqueued, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if !contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy released now that test has passed v2", enqueued)
	}
}

// TestPassedOnANonTriggeringGetStillReleases covers the shape a Concourse user
// writes by habit: one resource carries trigger:, the constrained one does
// not. The constrained resource used to be polled by nobody, so jobReadyFor
// never saw a version for it and held the job back forever — no error, no log
// line, no way to tell it apart from an idle watcher.
func TestPassedOnANonTriggeringGetStillReleases(t *testing.T) {
	dir := t.TempDir()

	repoVersions := dir + "/repo.json"
	artifactVersions := dir + "/artifacts.json"

	writeVersions(t, repoVersions, `[{"ref":"v1"}]`)
	writeVersions(t, artifactVersions, `[{"build":"1"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{
			{Name: "repo-listing", Config: config.ResourceTypeConfig{Check: "cat " + repoVersions, In: "echo fetched"}},
			{Name: "artifact-listing", Config: config.ResourceTypeConfig{Check: "cat " + artifactVersions, In: "echo fetched"}},
		},
		Resources: []config.Resource{
			{Name: "repo", Type: "repo-listing"},
			{Name: "artifacts", Type: "artifact-listing"},
		},
		Jobs: []config.Job{
			{
				Name: "build",
				Plan: []config.Step{
					{Get: "artifacts", Trigger: true},
					{Task: "make", Run: "true", Inputs: config.Inputs()},
				},
			},
			{
				Name: "deploy",
				Plan: []config.Step{
					{Get: "repo", Trigger: true},
					// No trigger: here — this get exists to constrain, not to fire.
					{Get: "artifacts", Passed: []string{"build"}},
					{Task: "ship", Run: "true", Inputs: config.Inputs()},
				},
			},
		},
	}

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	if !contains(Resources(cfg), "artifacts") {
		t.Fatal("a passed:-constrained resource is not polled, so its version can never be judged")
	}

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	writeVersions(t, repoVersions, `[{"ref":"v2"}]`)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// build goes green on the artifacts version deploy is constrained by.
	encoded, err := json.Marshal(map[string]any{"build": "1"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "build", "artifacts", string(encoded), "b1")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if !contains(enqueued, "deploy") {
		t.Errorf("enqueued = %v, want deploy released once build passed the artifacts version", enqueued)
	}
}

// TestConformancePassedRequiresVersionsToPassTogether pins the correlation
// property, which is the whole reason passed: exists on a fan-in.
//
// Concourse resolves passed: across a whole plan at once, against its versions
// DB, so the versions a downstream job runs with must have been green in the
// SAME upstream build (atc/scheduler/algorithm — its own unit tests need a
// real Postgres, which is why this is a transcribed scenario rather than a
// port; see docs/conformance.md).
//
// steps asked a weaker question until this test existed: "has this version
// been green in that job", per resource, independently. Two versions that each
// passed upstream in DIFFERENT builds satisfied it — so a downstream deploy
// could run a repo/config pair that was individually fine and jointly never
// tested. That is the bug this pins, and it was confirmed against the real
// store before being fixed.
//
// The scenario, which is the smallest one that distinguishes the two rules:
//
//	upstream build 1: repo=r2, config=c1  -> green
//	upstream build 2: repo=r1, config=c2  -> green
//
// r2 and c2 have each been green upstream. They have never been green
// together, so a downstream job constrained on both must NOT be released.
func TestConformancePassedRequiresVersionsToPassTogether(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	record := func(buildID, resource, ref string) {
		t.Helper()

		encoded, err := json.Marshal(map[string]any{"ref": ref})
		if err != nil {
			t.Fatal(err)
		}

		err = st.RecordPassedVersion(ctx, "upstream", resource, string(encoded), buildID)
		if err != nil {
			t.Fatalf("RecordPassedVersion: %v", err)
		}
	}

	record("build-1", "repo", "r2")
	record("build-1", "config", "c1")
	record("build-2", "repo", "r1")
	record("build-2", "config", "c2")

	crossed := map[string]map[string]any{
		"repo":   {"ref": "r2"},
		"config": {"ref": "c2"},
	}

	passed, err := pipeline.VersionSetPassedUpstream(ctx, st, "upstream", crossed)
	if err != nil {
		t.Fatalf("VersionSetPassedUpstream: %v", err)
	}

	if passed {
		t.Error("a combination that never passed upstream together satisfied the constraint — each version passed in a different build, and a fan-in accepting that runs code nothing validated")
	}

	// The control: a pair that DID pass together must still be released, or
	// the fix would just be a constraint nobody can satisfy.
	together := map[string]map[string]any{
		"repo":   {"ref": "r1"},
		"config": {"ref": "c2"},
	}

	passed, err = pipeline.VersionSetPassedUpstream(ctx, st, "upstream", together)
	if err != nil {
		t.Fatalf("VersionSetPassedUpstream: %v", err)
	}

	if !passed {
		t.Error("a combination that was green together in build-2 was refused")
	}
}

// TestConformancePassedRowsWithoutABuildCannotCorrelate covers the upgrade
// path, which is the one place this change can surprise someone.
//
// Rows written before build_id existed carry ”, and no correlated lookup can
// match them. So a multi-resource fan-in is held until its upstream job runs
// once more and writes a correlated set. That is the conservative direction —
// passed: is a gate, and waving through a combination nobody can prove was
// green together is the failure this whole change exists to prevent.
//
// A single-resource constraint is unaffected either way, which is why most
// pipelines will not notice: one row is trivially its own coherent set.
func TestConformancePassedRowsWithoutABuildCannotCorrelate(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())
	ctx := context.Background()

	encoded, err := json.Marshal(map[string]any{"ref": "v1"})
	if err != nil {
		t.Fatal(err)
	}

	// The empty build id is exactly what the ALTER leaves on a pre-upgrade row.
	err = st.RecordPassedVersion(ctx, "upstream", "repo", string(encoded), "")
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed, err := pipeline.VersionSetPassedUpstream(ctx, st, "upstream",
		map[string]map[string]any{"repo": {"ref": "v1"}})
	if err != nil {
		t.Fatalf("VersionSetPassedUpstream: %v", err)
	}

	if passed {
		t.Error("a row with no build id satisfied a correlated lookup; it cannot vouch for a combination, so it must not")
	}
}
