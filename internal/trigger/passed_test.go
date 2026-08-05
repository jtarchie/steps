package trigger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
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

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded))
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

	err = st.RecordPassedVersion(ctx, "unit", "repo", string(encoded))
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed, err := pipeline.VersionPassedUpstream(ctx, st, []string{"unit", "lint"}, "repo", version)
	if err != nil {
		t.Fatalf("VersionPassedUpstream: %v", err)
	}

	if passed {
		t.Error("a version green in one of two required jobs satisfied the constraint")
	}

	err = st.RecordPassedVersion(ctx, "lint", "repo", string(encoded))
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed, err = pipeline.VersionPassedUpstream(ctx, st, []string{"unit", "lint"}, "repo", version)
	if err != nil {
		t.Fatalf("VersionPassedUpstream: %v", err)
	}

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

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded))
	if err != nil {
		t.Fatalf("RecordPassedVersion: %v", err)
	}

	passed, err := pipeline.VersionPassedUpstream(ctx, st, []string{"test"}, "repo", map[string]any{"ref": "v2"})
	if err != nil {
		t.Fatalf("VersionPassedUpstream: %v", err)
	}

	if passed {
		t.Error("a job green on v1 released v2; passed: is per version, not per job")
	}
}

func contains(names []string, want string) bool {
	return strings.Contains(strings.Join(names, ","), want)
}
