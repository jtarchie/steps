package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/jtarchie/steps/internal/venue"
)

// workersKey types the context value carrying the tag-to-worker mapping.
type workersKey struct{}

// WithWorkers records the --worker mappings for this invocation, rejecting any
// that cannot be reached as written.
//
// Parsed here rather than at first use so a typo in a worker URL is reported
// before a run starts, alongside every other thing that is wrong with the
// invocation, rather than mid-plan when a step happens to reach for it.
func WithWorkers(ctx context.Context, mappings map[string]string) (context.Context, error) {
	if len(mappings) == 0 {
		return ctx, nil
	}

	workers := make(map[string]venue.Worker, len(mappings))

	for _, tag := range slices.Sorted(maps.Keys(mappings)) {
		worker, err := venue.ParseWorker(mappings[tag])
		if err != nil {
			return nil, fmt.Errorf("--worker %s: %w", tag, err)
		}

		workers[tag] = worker
	}

	return context.WithValue(ctx, workersKey{}, workers), nil
}

func workersFrom(ctx context.Context) map[string]venue.Worker {
	workers, _ := ctx.Value(workersKey{}).(map[string]venue.Worker)

	return workers
}

// leasesKey types the context value carrying one job's venue leases.

// registryKey types the context value carrying the process's shared machines.
type registryKey struct{}

// WithWorkerRegistry makes the last of a daemon's jobs, polls and webhooks out give a machine back, rather than the first; its release belongs after every loop that could hold one has stopped.
func WithWorkerRegistry(ctx context.Context, owner string) (context.Context, func()) {
	return withRegistry(ctx, venue.NewRegistry(owner))
}

// SharesWorkers is what a daemon has to arrange, and what its test asserts: without it every scope owns its machine outright again.
func SharesWorkers(ctx context.Context) bool {
	_, shared := ctx.Value(registryKey{}).(*venue.Registry)

	return shared
}

// WorkerOwner is the id the daemon's registry stamps on the machines it starts, empty with no registry.
func WorkerOwner(ctx context.Context) string {
	registry, _ := ctx.Value(registryKey{}).(*venue.Registry)
	if registry == nil {
		return ""
	}

	return registry.Owner()
}

// SweepWorkers gives back what a previous life of this daemon left running, so it belongs before any loop starts: nothing can be using a machine yet.
func SweepWorkers(ctx context.Context) {
	registry, _ := ctx.Value(registryKey{}).(*venue.Registry)
	if registry == nil {
		return
	}

	// Cancellable, unlike a release: a listing interrupted by shutdown gives nothing back, and each give-back already runs on its own context.
	sweepCtx, cancel := context.WithTimeout(ctx, WorkerReleaseTimeout)
	defer cancel()

	err := registry.Sweep(sweepCtx, workersFrom(ctx))
	if err != nil {
		slog.ErrorContext(ctx, "worker.sweep_failed", "error", err,
			"consequence", "a machine this daemon started before it restarted may still be running and billing")
	}
}

func withRegistry(ctx context.Context, registry *venue.Registry) (context.Context, func()) {
	return context.WithValue(ctx, registryKey{}, registry), func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WorkerReleaseTimeout)
		defer cancel()

		err := registry.Close(releaseCtx)
		if err != nil {
			slog.ErrorContext(ctx, "worker_release_failed", "error", err)
			warnf(ctx, "a worker acquired by this process could not be released: %v", err)
		}
	}
}
