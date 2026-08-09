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
