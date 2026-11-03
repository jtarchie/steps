package pipeline

import (
	"context"
	"log/slog"

	"github.com/jtarchie/steps/internal/venue"
)

// leasesKey types the context value carrying one job's venue leases.
type leasesKey struct{}

// jobLeases is per job, never per step, because acquisition costs 20-90 seconds and real money; with no registry installed, the scope's own end gives everything back.
func jobLeases(ctx context.Context) (context.Context, func(context.Context)) {
	return withLeases(ctx, (*venue.Registry).Leases)
}

// CheckScope scopes one round of a poll's checks the way RunJob scopes a job, so a placed check resolves its worker through the same registry a job's steps do — which is what lets a poll end without stopping a machine a job is on — except that on an exclusive pool it rides along on a machine a job holds rather than waiting for one of its own.
func CheckScope(ctx context.Context) (context.Context, func()) {
	ctx, release := placedScope(ctx, (*venue.Registry).CheckLeases)

	return ctx, func() { release(ctx) }
}

// placedScope installs a scope's leases with resource placement under them, and a release bounded under a context of its own, because a cancelled scope is exactly when a machine left running would be worst.
func placedScope(ctx context.Context, open func(*venue.Registry, map[string]venue.Worker) *venue.Leases) (context.Context, func(context.Context)) {
	ctx, release := withLeases(ctx, open)

	return WithResourcePlacement(ctx), func(ctx context.Context) {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WorkerReleaseTimeout)
		defer cancel()

		release(releaseCtx)
	}
}

func withLeases(ctx context.Context, open func(*venue.Registry, map[string]venue.Worker) *venue.Leases) (context.Context, func(context.Context)) {
	var leases *venue.Leases

	if registry, shared := ctx.Value(registryKey{}).(*venue.Registry); shared {
		leases = open(registry, workersFrom(ctx))
	} else {
		leases = venue.NewLeases(workersFrom(ctx))
	}

	return context.WithValue(ctx, leasesKey{}, leases), func(ctx context.Context) {
		err := leases.ReleaseAll(ctx)
		if err != nil {
			// Logged, never returned: a job that succeeded did succeed, and
			// a machine that could not be given back is an operational
			// problem rather than a wrong answer. It is loud because it
			// costs money for as long as nobody notices.
			slog.ErrorContext(ctx, "job.worker_release_failed", "error", err)
			warnf(ctx, "a worker acquired for this job could not be released: %v", err)
		}
	}
}

func leasesFrom(ctx context.Context) *venue.Leases {
	leases, _ := ctx.Value(leasesKey{}).(*venue.Leases)

	return leases
}
