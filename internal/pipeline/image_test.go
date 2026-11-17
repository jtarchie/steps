package pipeline

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func imageTestConfig() *config.Config {
	return &config.Config{
		Resources: []config.Resource{{Name: "toolchain", Source: map[string]any{"repository": "ghcr.io/me/toolchain"}}},
		Jobs: []config.Job{{Name: "build", Plan: []config.Step{
			{Get: "toolchain"},
			// Placed, so resolving pulls nothing on this machine.
			{Try: &config.Step{Task: "t", Run: "true", Image: "toolchain", Tags: []string{"remote"}}},
		}}},
	}
}

// TestResolveStepImageNeverWritesTheLoadedConfig is the hazard a long-lived
// daemon hits: one loaded Config serves every run, so a digest written into it
// would be the next run's image — stale, and a false cache hit.
func TestResolveStepImageNeverWritesTheLoadedConfig(t *testing.T) {
	t.Parallel()

	cfg := imageTestConfig()

	for _, digest := range []string{"sha256:" + strings.Repeat("1", 64), "sha256:" + strings.Repeat("2", 64)} {
		ctx, _ := withBuildVersions(context.Background())
		recordFetched(ctx, "toolchain", cfg.Resources[0].Source, map[string]any{"digest": digest})

		got, err := resolveStepImage(ctx, cfg, "build", cfg.Jobs[0].Plan[1])
		if err != nil {
			t.Fatalf("resolveStepImage: %v", err)
		}

		if want := "ghcr.io/me/toolchain@" + digest; got.Unwrap().Image != want {
			t.Errorf("resolved image = %q, want %q", got.Unwrap().Image, want)
		}

		if image := cfg.Jobs[0].Plan[1].Try.Image; image != "toolchain" {
			t.Fatalf("the loaded config's image became %q", image)
		}
	}
}

// TestResolveStepImageWhileGetsRecord runs under -race: in_parallel branches
// and across: cells resolve while other gets in the build still record.
func TestResolveStepImageWhileGetsRecord(t *testing.T) {
	t.Parallel()

	cfg := imageTestConfig()
	ctx, _ := withBuildVersions(context.Background())
	recordFetched(ctx, "toolchain", cfg.Resources[0].Source, map[string]any{"digest": "sha256:" + strings.Repeat("1", 64)})

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			recordFetched(ctx, "other", map[string]any{}, map[string]any{"ref": "v"})
		})
		wg.Go(func() {
			_, err := resolveStepImage(ctx, cfg, "build", cfg.Jobs[0].Plan[1])
			if err != nil {
				t.Errorf("resolveStepImage: %v", err)
			}
		})
	}

	wg.Wait()
}

// TestRefuseRenderedArtifactImage: an artifact name that arrives through a
// load_var: is past resolution, so it is refused rather than pulled as-is.
func TestRefuseRenderedArtifactImage(t *testing.T) {
	t.Parallel()

	cfg := imageTestConfig()

	err := refuseRenderedArtifactImage(cfg, "build", config.Step{Task: "t", Image: "toolchain"})
	if err == nil || !strings.Contains(err.Error(), "load_var") {
		t.Fatalf("err = %v, want the load_var refusal", err)
	}

	err = refuseRenderedArtifactImage(cfg, "build", config.Step{Task: "t", Image: "alpine:3"})
	if err != nil {
		t.Fatalf("err = %v for a literal image", err)
	}
}
