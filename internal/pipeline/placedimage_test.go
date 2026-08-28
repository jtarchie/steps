package pipeline

// The preflight a PLACED containerized step still owes this machine.

import (
	"context"
	"strings"
	"testing"
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
`, "build")

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	// A worker nothing can reach, so the run has to get as far as dialling for
	// its error to say so — which is exactly what distinguishes "went further
	// than preflight" from "passed for the right reason by accident".
	ctx, err := WithWorkers(context.Background(), map[string]string{"box": "ssh://nobody@127.0.0.1:1"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	t.Setenv("PATH", t.TempDir()) // empty PATH: docker cannot be found

	err = RunJob(ctx, cfg, job, nil, provider, st, false)
	if err == nil {
		t.Fatal("the unreachable worker was never dialled; this test can no longer tell how far the job got")
	}

	if strings.Contains(err.Error(), "docker CLI not found on PATH") {
		t.Errorf("error = %v; a placed containerized step needs no docker binary on this machine", err)
	}
}
