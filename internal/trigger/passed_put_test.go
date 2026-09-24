package trigger

// A version a job PUT counts as having passed it, carried across the seam from
// the put, through job_versions, to the poller's release and the downstream
// build's resolution.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/workspace"
)

func TestConformancePassedCountsAVersionTheUpstreamPut(t *testing.T) {
	dir := t.TempDir()
	shipped := filepath.Join(dir, "shipped.txt")

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: image
  config:
    check: echo '[{"digest":"from-check"}]'
    in: echo {{ .version.digest | shellquote }} > digest.txt
    out: echo '{"digest":"sha-1"}'
resources:
- name: toolchain
  type: image
  source: {}
jobs:
- name: build-toolchain
  plan:
  - put: toolchain
- name: implement
  plan:
  - get: toolchain
    trigger: true
    passed: [build-toolchain]
  - task: ship
    inputs: [toolchain]
    run: cat toolchain/digest.txt >> `+shipped+`
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	err = pipeline.RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob build-toolchain: %v", err)
	}

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if !contains(enqueued, "implement") {
		t.Fatalf("enqueued = %v, want implement released by the version build-toolchain put", enqueued)
	}

	runQueued(ctx, t, cfg, st, false)

	data, err := os.ReadFile(shipped) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("implement never ran: %v", err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "sha-1" {
		t.Errorf("implement built %v, want [sha-1] — the put's version, not the check's", got)
	}
}

// TestConformancePassedCorrelatesAPutWithItsBuildsGets: a put's version is
// recorded under the build that made it, so a fan-in over the get and the put
// asks about the pair that ran together and no other.
func TestConformancePassedCorrelatesAPutWithItsBuildsGets(t *testing.T) {
	dir := t.TempDir()

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: echo '[{"ref":"r1"},{"ref":"r2"}]'
    in: echo {{ .version.ref | shellquote }} > ref.txt
    out: echo "{\"ref\":\"img-$(cat repo/ref.txt)\"}"
resources:
- name: repo
  type: feed
  source: {}
- name: image
  type: feed
  source: {}
jobs:
- name: upstream
  plan:
  - get: repo
    version: every
  - put: image
    inputs: [repo]
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	err = pipeline.RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	for _, tc := range []struct {
		repo, image string
		want        bool
	}{
		{"r2", "img-r2", true},
		{"r1", "img-r1", true},
		{"r1", "img-r2", false},
	} {
		got, err := pipeline.VersionSetPassedUpstream(ctx, st, "upstream", map[string]map[string]any{
			"repo":  {"ref": tc.repo},
			"image": {"ref": tc.image},
		})
		if err != nil {
			t.Fatal(err)
		}

		if got != tc.want {
			t.Errorf("passed{repo:%s image:%s} = %v, want %v", tc.repo, tc.image, got, tc.want)
		}
	}
}

// TestConformancePassedCorrelatesAJobHookPutWithTheBuildsGets: a put in a
// job-level on_success hook is an output of the build it followed. Recorded
// under the run alone, it shared no build id with the gets, and a downstream
// fan-in over both was held back forever.
func TestConformancePassedCorrelatesAJobHookPutWithTheBuildsGets(t *testing.T) {
	dir := t.TempDir()

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: echo '[{"ref":"r1"}]'
    in: "true"
    out: echo '{"ref":"img"}'
resources:
- name: repo
  type: feed
  source: {}
- name: image
  type: feed
  source: {}
jobs:
- name: upstream
  plan:
  - get: repo
  on_success:
    put: image
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	err = pipeline.RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	got, err := pipeline.VersionSetPassedUpstream(ctx, st, "upstream", map[string]map[string]any{
		"repo":  {"ref": "r1"},
		"image": {"ref": "img"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !got {
		t.Error("the hook's put does not correlate with the build's get")
	}
}
