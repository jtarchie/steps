package venue

// Bringing a tree home from the worker that holds it (steps#138, rung 2): a deferred fetch left a step's outputs on a docker+ worker, filed under their digests, and this lands one where the caller says, checked against its digest.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/shell"
)

// Pull fills dst, an existing empty directory, with the tree the worker spec names holds under digest, and reports the bytes that crossed. A worker that no longer holds it is an error naming the worker; the caller decides what a lost holder costs.
func Pull(ctx context.Context, spec shell.RunnerSpec, _, digest, dst string) (int64, error) {
	worker, err := ParseWorker(spec.Worker)
	if err != nil {
		return 0, err
	}

	if !worker.dockerPlus() {
		return 0, fmt.Errorf("worker %q: %w", spec.Worker, errHoldsNothing)
	}

	received, err := pullPlus(ctx, worker, digest, dst)
	if err != nil {
		return 0, fmt.Errorf("worker %q: %w", spec.Worker, err)
	}

	return received, nil
}

// errHoldsNothing is a Pull from a worker that keeps nothing between steps: an ssh:// worker brings every output home.
var errHoldsNothing = errors.New("an ssh:// worker holds nothing to pull")
