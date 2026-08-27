package config

import (
	"strings"
	"testing"
)

// TestImagesCollectsEveryDistinctImage covers what the pre-pull walks: every
// place image: can be set, deduped, so a pipeline naming one image in four
// places pulls it once.
func TestImagesCollectsEveryDistinctImage(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{Name: "rt", Image: "alpine:3"}},
		Agents:        []Agent{{Name: "a", Image: "python:3.12"}},
		Tasks:         []Task{{Name: "t", Image: "alpine:3"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "t", Run: "true", Image: "golang:1.26"},
			{Task: "t", Run: "true"},
		}}},
	}

	got := cfg.Images()

	want := []string{"alpine:3", "golang:1.26", "python:3.12"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Images() = %v, want %v (sorted and deduped)", got, want)
	}
}

// TestImagesIsEmptyForAHostOnlyPipeline is what keeps a pipeline that never
// containerizes from touching docker at all.
func TestImagesIsEmptyForAHostOnlyPipeline(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "t", Run: "true"}},
		Jobs:  []Job{{Name: "j", Plan: []Step{{Task: "t"}}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none", got)
	}

	if cfg.UsesImages() {
		t.Error("UsesImages() = true for a pipeline that sets no image:")
	}
}

// TestImagesSkipsAPlacedStepsImage keeps a worker's image off this machine's
// daemon.
//
// A placed step's container runs on the WORKER's daemon, which does not exist
// yet when this is asked — a machine acquired for the job has not been
// acquired. Collecting it here made prepareImages demand a local daemon for a
// pipeline whose every container lives elsewhere, and pull the image to a
// machine that will never run it — worse, `docker image inspect` finding a
// LOCALLY built tag skipped the pull the worker was the one that needed.
func TestImagesSkipsAPlacedStepsImage(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "remote", Image: "alpine:3", Tags: []string{"box"}, Run: "true"},
		}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none — that image runs on the worker's daemon", got)
	}

	if cfg.UsesImages() {
		t.Error("UsesImages() = true, so the job demands a local daemon it never uses")
	}

	// And the same image on a step that is NOT placed is still pulled here,
	// which is the behaviour this must not cost.
	cfg.Jobs[0].Plan = append(cfg.Jobs[0].Plan, Step{Task: "local", Image: "alpine:3", Run: "true"})

	if got := cfg.Images(); len(got) != 1 || got[0] != "alpine:3" {
		t.Errorf("Images() = %v, want the un-placed step's image", got)
	}
}

// TestImagesSkipsATaskEntryOnlyPlacedStepsUse pins the other half of the
// placed-step rule.
//
// Images() is what the orchestrator pre-pulls and what makes it demand a
// local daemon. A step's own image: is already skipped when it carries tags:,
// because that container runs on the worker — but a tasks: entry is visited
// on its own, knowing nothing about who references it, so an image reached
// only through a tagged step was still pulled here. On a machine with no
// daemon that refused the job outright; with one, it pulled an image that
// will never run — and for a locally BUILT tag, found it, skipped the pull,
// and left the worker to fail with "Unable to find image".
func TestImagesSkipsATaskEntryOnlyPlacedStepsUse(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "remote", Image: "alpine:3", Run: "true"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "remote", Tags: []string{"box"}},
		}}},
	}

	if got := cfg.Images(); len(got) != 0 {
		t.Errorf("Images() = %v, want none — that image runs on the worker", got)
	}
}

// TestImagesKeepsATaskEntryAnyLocalStepUses is the guard on the guard: one
// untagged reference is enough to need the image here.
func TestImagesKeepsATaskEntryAnyLocalStepUses(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "both", Image: "alpine:3", Run: "true"}},
		Jobs: []Job{{Name: "j", Plan: []Step{
			{Task: "both", Tags: []string{"box"}},
			{Task: "both"},
		}}},
	}

	got := cfg.Images()
	if len(got) != 1 || got[0] != "alpine:3" {
		t.Errorf("Images() = %v, want alpine:3 — a local step still runs it here", got)
	}
}
