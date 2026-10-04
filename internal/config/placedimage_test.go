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
