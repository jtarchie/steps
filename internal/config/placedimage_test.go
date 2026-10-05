package config

import "testing"

// A try: wrapper is visited alongside what it wraps and carries its tag, so it must answer the wrapped step's image: an empty answer refused try-wrapped image steps on docker+ workers.
func TestPlacedImageSeesThroughTry(t *testing.T) {
	t.Parallel()

	step := Step{Try: &Step{Image: "alpine"}}

	got := (&Config{}).PlacedImage(step)
	if got != "alpine" {
		t.Errorf("PlacedImage = %q, want the wrapped step's image", got)
	}
}

// A put names its resource with resource: when the step is named otherwise; looking it up by the step's name found nothing and refused the put on a docker+ worker.
func TestPlacedImageFindsAPutsRenamedResource(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{Name: "probe", Image: "alpine"}},
		Resources:     []Resource{{Name: "repo", Type: "probe"}},
	}

	got := cfg.PlacedImage(Step{Put: "publish", Resource: "repo"})
	if got != "alpine" {
		t.Errorf("PlacedImage = %q, want the resource type's image", got)
	}
}
