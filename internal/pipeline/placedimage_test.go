package pipeline

// The preflight a PLACED containerized step still owes this machine.

import (
	"context"
	"strings"
	"testing"
)

// TestPlacedImageNeedsTheDockerCLIBeforeAWorkerIsDialled crosses the seam
// between config's narrowing and the fail-fast contract.
//
// Images() deliberately omits a placed step's image — that container runs on
// the worker's daemon, and an orchestrator with no daemon at all is the
// arrangement the feature exists for. But gating the WHOLE docker preflight on
// Images() dropped the CLI check with the daemon check, and the placed
// containerized path is driven by this machine's docker binary aimed at the
// forwarded socket. With no docker on PATH the job sailed through preflight,
// launched and billed a machine, pushed the tree, and only then died inside
// the step on `exec: "docker": executable file not found in $PATH`.
func TestPlacedImageNeedsTheDockerCLIBeforeAWorkerIsDialled(t *testing.T) {
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

	// A worker nothing can reach, so a run that got as far as dialling says so
	// in its error rather than passing for the right reason by accident.
	ctx, err := WithWorkers(context.Background(), map[string]string{"box": "ssh://nobody@127.0.0.1:1"})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	t.Setenv("PATH", t.TempDir()) // empty PATH: docker cannot be found

	err = RunJob(ctx, cfg, job, nil, provider, st, false)
	if err == nil {
		t.Fatal("a placed image: ran to completion with no docker CLI on PATH")
	}

	if !strings.Contains(err.Error(), "docker CLI not found on PATH") {
		t.Errorf("error = %v, want the missing docker CLI reported before the worker was dialled", err)
	}
}
