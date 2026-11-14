package pipeline

// The preflight a PLACED containerized step still owes this machine.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/venue"
)

// TestPlacedImageNeedsNoDockerOnTheOrchestrator is the inverse of the test it
// replaces, and the inversion is the point.
//
// Images() deliberately omits a placed step's image — that container runs on
// the worker's daemon, and an orchestrator with no daemon at all is the
// arrangement the feature exists for. But the container used to be STARTED by
// this machine's docker binary aimed at the forwarded socket, so the binary
// had to be here even though the daemon did not, and a missing one had to be
// caught before a machine was acquired and billed and the tree pushed.
//
// The container is created over the socket directly now. There is nothing
// this machine has to have, so the whole failure mode is gone rather than
// moved: a job with no docker anywhere on PATH gets as far as dialling the
// worker, and fails — if it fails — about the worker.
func TestPlacedImageNeedsNoDockerOnTheOrchestrator(t *testing.T) {
	// Not t.Parallel(): strips PATH for the whole process.
	cfg, job, st, provider := fixtureFrom(t, `
jobs:
  - name: build
    plan:
      - task: remote
        tags: [box]
        image: alpine:3
        run: "true"
`)

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	// A worker nothing can reach, so the run has to get as far as dialling for
	// its error to say so — which is exactly what distinguishes "went further
	// than preflight" from "passed for the right reason by accident".
	ctx, err := WithWorkers(context.Background(), map[string]string{"box": "docker+ssh://nobody@127.0.0.1:1"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	// ssh:// refuses image: here, before any dial, which once let this test pass without the run getting anywhere near the worker.
	err = ValidateWorkerPlacement(ctx, cfg, job)
	if err != nil {
		t.Fatalf("preflight refused the placement, so the run cannot reach the dial: %v", err)
	}

	t.Setenv("PATH", t.TempDir()) // empty PATH: docker cannot be found

	err = RunJob(ctx, cfg, job, st, Run{Provider: provider})
	if err == nil {
		t.Fatal("the unreachable worker was never dialled; this test can no longer tell how far the job got")
	}

	if strings.Contains(err.Error(), "docker CLI not found on PATH") {
		t.Errorf("error = %v; a placed containerized step needs no docker binary on this machine", err)
	}
}

// TestLocalImagesBringTheHolderImage: an untagged image step's tree goes through a holder container, so its image is pulled with the step's, before any step, and never inside one's timeout: (#246).
func TestLocalImagesBringTheHolderImage(t *testing.T) {
	t.Parallel()

	cfg, _, st, provider := fixtureFrom(t, `
jobs:
  - name: build
    plan:
      - task: local
        image: alpine:3
        run: "true"
`)

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	got := imagesToPrepare(cfg)
	if !slices.Contains(got, "alpine:3") || !slices.Contains(got, venue.HolderImage) {
		t.Errorf("imagesToPrepare = %v, want the step's image and %s", got, venue.HolderImage)
	}

	none, _, st2, provider2 := fixtureFrom(t, "jobs: [{name: build, plan: [{task: t, run: \"true\"}]}]\n")

	defer func() { _ = st2.Close() }()
	defer func() { _ = provider2.Close() }()

	if got := imagesToPrepare(none); len(got) != 0 {
		t.Errorf("imagesToPrepare with no image: = %v, want nothing to pull", got)
	}
}
