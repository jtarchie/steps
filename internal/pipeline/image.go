package pipeline

// A step image: naming an artifact an earlier get fetched.

import (
	"context"
	"fmt"
	"maps"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

// resolveStepImage replaces an image: naming an artifact with the reference
// this build's get of it fetched (config.ResolveArtifactImage), before the
// step is guarded, hashed or run — the guard runs in the step's image, and
// the reference is what the cache must key on.
//
// A LOCAL step's image is pulled here, outside the step, so a cold pull of a
// large toolchain is not charged to the step's timeout: or attempts:, the
// same reason Images() is pulled up front for a fixed image. A placed step's
// worker pulls on create. A pull failure is an infrastructure error, never
// the step saying no.
func resolveStepImage(ctx context.Context, cfg *config.Config, jobName string, step config.Step) (config.Step, error) {
	// Every non-get dispatch lands here — across: cells, block children,
	// hooks — so the build's fetched map is only snapshotted for a step that
	// names an artifact.
	artifacts := cfg.ImageArtifacts(jobName)
	if !artifacts[step.Unwrap().Image] {
		return step, nil
	}

	resolved, ok, err := config.ResolveArtifactImage(step, artifacts, fetchedImages(ctx))
	if err != nil {
		return step, fmt.Errorf("job %q: %w", jobName, err)
	}

	if !ok {
		return step, nil
	}

	reference := resolved.Unwrap().Image
	events.Note(ctx, events.NoteInfo, fmt.Sprintf("image: %s → %s", step.Unwrap().Image, reference))

	if placementTag(resolved) == "" {
		err = shell.PrepareImages(ctx, []string{reference})
		if err != nil {
			return step, fmt.Errorf("job %q: image %q: %w", jobName, step.Unwrap().Image, err)
		}
	}

	return resolved, nil
}

// fetchedImages snapshots what this build's gets fetched. A snapshot, because
// in_parallel branches, across: cells and sibling builds keep recording.
func fetchedImages(ctx context.Context) map[string]config.Fetched {
	versions, ok := ctx.Value(buildVersionsKey{}).(*buildVersions)
	if !ok {
		return nil
	}

	versions.mu.Lock()
	defer versions.mu.Unlock()

	fetched := make(map[string]config.Fetched, len(versions.fetched))

	for get, version := range versions.fetched {
		fetched[get] = config.Fetched{Source: maps.Clone(versions.sources[get]), Version: maps.Clone(version)}
	}

	return fetched
}

// refuseRenderedArtifactImage refuses an image: that became an artifact name
// only once a load_var: rendered into it. Resolution has already run by then
// — it must, the guard runs in the image — so the name would otherwise be
// pulled from Docker Hub as it stands.
func refuseRenderedArtifactImage(cfg *config.Config, jobName string, step config.Step) error {
	if step.Image == "" || !cfg.ImageArtifacts(jobName)[step.Image] {
		return nil
	}

	return fmt.Errorf("job %q: image %q names an artifact through a load_var:, which is not supported; name the get directly", jobName, step.Image)
}
