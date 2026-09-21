package pipeline

// The crossing between resolving a worker and dialling it.

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// TestPlaceAgentSpecCarriesTheResolvedWorker is the seam, tested as a seam. The lease resolving a worker and the dialer parsing a URL were each well covered when the venue work shipped a step that dialled nothing, because no test carried the resolved worker ACROSS. internal/agent cannot know what a tag means, so this is the only place the mapping and the runner meet.
func TestPlaceAgentSpecCarriesTheResolvedWorker(t *testing.T) {
	t.Parallel()

	ctx, err := WithWorkers(t.Context(), map[string]string{"box": "local:"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	step := config.Step{Agent: "hand", Tags: []string{"box"}}

	// Only what the agent itself knows goes in; what comes out is the closure's whole contribution.
	got, err := placeAgentSpec(ctx, step, shell.RunnerSpec{Image: "alpine:3", Cwd: t.TempDir(), Subdir: "code"})
	if err != nil {
		t.Fatalf("placeAgentSpec: %v", err)
	}

	if got.Worker != "local:" {
		t.Errorf("Worker = %q, want the machine the tag resolved to — the agent's runner would dial nothing", got.Worker)
	}

	if got.WorkerTag != "box" {
		t.Errorf("WorkerTag = %q, want %q so the placement record names the mapping that chose the machine", got.WorkerTag, "box")
	}

	if !got.NoRedial {
		t.Error("NoRedial is unset; a worker lost mid-conversation would be redialled and the model's edits rewound under it")
	}

	if got.Image != "alpine:3" || got.Subdir != "code" {
		t.Errorf("Image/Subdir = %q/%q, want the agent's own to survive the crossing", got.Image, got.Subdir)
	}
}

// TestPlaceAgentSpecLeavesAnUntaggedStepAlone keeps the common case honest: with no tags: nothing is added, or every local agent would start dialling a venue.
func TestPlaceAgentSpecLeavesAnUntaggedStepAlone(t *testing.T) {
	t.Parallel()

	ctx, err := WithWorkers(t.Context(), map[string]string{"box": "local:"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	got, err := placeAgentSpec(ctx, config.Step{Agent: "hand"}, shell.RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("placeAgentSpec: %v", err)
	}

	if got.Worker != "" || got.WorkerTag != "" {
		t.Errorf("Worker/WorkerTag = %q/%q, want empty for a step that named no tag", got.Worker, got.WorkerTag)
	}
}
