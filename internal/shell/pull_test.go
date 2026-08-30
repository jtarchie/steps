package shell

// What gets pulled, and what does not.
//
// These used to drive a fake `docker` on PATH and assert its argv. Preparing
// images no longer spawns anything, so the questions are asked of a real
// daemon instead — which is the better oracle anyway: the argv only ever
// proved what the CLI was told.

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPrepareImagesAnnouncesOnlyTheMissingOne is the property that keeps this
// cheap enough to run before every job, including every job `steps web`
// fires: an image already on the daemon costs one local lookup and no network.
//
// Asserted through the announcement, because that is the only externally
// visible difference between checking and pulling — and because the clock is
// not one: a re-pull of an image whose layers are all cached returns in
// milliseconds.
func TestPrepareImagesAnnouncesOnlyTheMissingOne(t *testing.T) {
	requireDocker(t)
	requireImagePresent(t, testImage)

	const missing = "steps-test-no-such-image:definitely-not-here"

	announced := captureStdout(t, func() {
		err := PrepareImages(context.Background(), []string{testImage, missing})
		if err == nil {
			t.Error("PrepareImages succeeded with an image that cannot be pulled")
		}
	})

	if strings.Contains(announced, testImage) {
		t.Errorf("announced %q; the image already on the daemon must not be pulled", strings.TrimSpace(announced))
	}

	if !strings.Contains(announced, missing) {
		t.Errorf("announced %q, want the missing image named", strings.TrimSpace(announced))
	}
}

// TestPrepareImagesLocallyBuiltImageIsNotPulled covers the case the presence
// check exists for, with an image that genuinely exists in no registry: a
// pull of it CANNOT succeed, so this fails loudly rather than subtly if the
// check ever regresses.
func TestPrepareImagesLocallyBuiltImageIsNotPulled(t *testing.T) {
	requireDocker(t)
	requireImagePresent(t, testImage)

	const local = "steps-test-local-build:dev"

	tagImage(t, testImage, local)

	err := PrepareImages(context.Background(), []string{local})
	if err != nil {
		t.Fatalf("PrepareImages: %v; an image built on this machine has no registry to be pulled from", err)
	}
}

// tagImage gives an existing image a second name that exists only here, and
// removes it afterwards.
func tagImage(t *testing.T, source, target string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	//nolint:gosec // both names are constants in this package
	out, err := exec.CommandContext(ctx, "docker", "tag", source, target).CombinedOutput()
	if err != nil {
		t.Fatalf("tagging %s as %s: %v\n%s", source, target, err, out)
	}

	t.Cleanup(func() {
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer removeCancel()

		//nolint:gosec // a constant in this package
		_ = exec.CommandContext(removeCtx, "docker", "rmi", target).Run()
	})
}

// TestPrepareImagesNoImagesNeedsNoDaemon pins that a pipeline naming no image
// never asks about one.
//
// Aimed at an address nothing is listening on, which a run that touched the
// daemon at all could not survive. The case is ordinary: a pipeline of host
// tasks on a machine with no docker at all has to work.
func TestPrepareImagesNoImagesNeedsNoDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")

	err := PrepareImages(context.Background(), nil)
	if err != nil {
		t.Errorf("PrepareImages: %v, want an empty image list to need nothing", err)
	}
}

// TestPrepareImagesTreatsAFlagShapedNameAsAnImage is what replaced the `--`
// separator this used to assert.
//
// The old shape built an argv, so an image value docker's own parser would
// read as a flag had to be pushed behind a positional separator. An image name
// is now a field in a JSON request and cannot be read as an option by
// anything — the defense is structural rather than positional. What is worth
// keeping is the OUTCOME: such a value is looked up as the (invalid) image
// name it is, and never changes how the daemon is asked.
func TestPrepareImagesTreatsAFlagShapedNameAsAnImage(t *testing.T) {
	requireDocker(t)

	err := PrepareImages(context.Background(), []string{"--privileged"})
	if err == nil {
		t.Fatal("PrepareImages succeeded for an image named like a flag")
	}

	if !strings.Contains(err.Error(), "--privileged") {
		t.Errorf("error = %v, want the value reported as the image name it is", err)
	}
}
