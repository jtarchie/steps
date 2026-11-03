package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// fetchGetVersions resolves a get step's versions with retries and timeout
// support, returning the resource, its type, and the versions to fetch.
func fetchGetVersions(ctx context.Context, cfg *config.Config, step config.Step, pinned map[string]string, cache *rsrc.Cache) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	var (
		resource     *config.Resource
		resourceType *config.ResourceType
		versions     []map[string]any
	)

	err := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		notef(ctx, "get: %s (attempt %d/%d)", step.Get, attempt, total)
		slog.InfoContext(ctx, "job.get.attempt", "get", step.Get, "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		res, resType, vers, fetchErr := cache.ResolveVersionsCached(attemptCtx, cfg, step, pinned)
		if fetchErr != nil {
			return fetchErr //nolint:wrapcheck // wrapped with get context by the caller below
		}

		resource, resourceType, versions = res, resType, vers

		return nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	// A nil pair with no error would mean the resolver returned success
	// without resolving anything. Nothing does that today; saying so here
	// turns a would-be nil dereference in the caller into a named failure.
	if resource == nil || resourceType == nil {
		return nil, nil, nil, fmt.Errorf("get %q: resolved to no resource", step.Get)
	}

	return resource, resourceType, versions, nil
}

// fetchGetStepWithStep places one version of a resource into bw's resource
// directory, with the step's retry/timeout applied. The directory — and thus
// the artifact downstream steps name as an input — is always the get step's
// artifact name (its get: value), which differs from the resource when the get
// aliases it via resource:; only the fetched content comes from the resource.
func fetchGetStepWithStep(ctx context.Context, cfg *config.Config, st store.Deliveries, step config.Step, artifact string, resource config.Resource, resourceType config.ResourceType, version map[string]any, bw workspace.BuildWorkspace) error {
	// The venue retry wraps the attempts: loop, as a task's does — see
	// runPlacedStage.
	err := runPlacedStage(ctx, step, func(ctx context.Context) error {
		return retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
			notef(ctx, "get: %s (version: %s, attempt %d/%d)", artifact, versionText(version), attempt, total)
			slog.InfoContext(ctx, "job.get.in.attempt", "artifact", artifact, "attempt", attempt, "total_attempts", total)
		}, func(attemptCtx context.Context) error {
			err := fetchGetStep(attemptCtx, cfg, st, artifact, resource, resourceType, version, step.Params, bw)

			// An eviction ends the attempts loop rather than spending it —
			// the machine is gone, and the venue retry is what re-places.
			if errors.Is(err, venue.ErrEvicted) {
				return retry.Stop(err)
			}

			return err
		})
	})
	if err != nil {
		return fmt.Errorf("get %q: %w", artifact, err)
	}

	// Before the get's hooks run, so on_success can read what it fetched.
	recordFetched(ctx, artifact, resource.Source, version)

	return nil
}

func fetchGetStep(ctx context.Context, cfg *config.Config, st store.Deliveries, artifact string, resource config.Resource, resourceType config.ResourceType, version, params map[string]any, bw workspace.BuildWorkspace) error {
	notef(ctx, "get: %s (version: %s)", artifact, versionText(version))

	fetch := func(dir string) error {
		runCtx, recorder := recordResourceOutput(ctx, resourceType.Name+" in")
		err := rsrc.RunIn(runCtx, rsrc.Call{Config: cfg, Type: resourceType, Resource: resource, Version: version, Params: params, Dir: dir})
		recorder.finish(ctx)

		if err != nil {
			return err //nolint:wrapcheck // the caller classifies and names the resource
		}

		// The stage closed its own runner, so what the worker kept comes out
		// through the sink runPlacedStage installed; recorded inside the
		// fetch, before the resource cache looks at the directory it filled.
		held, holder := heldFrom(ctx)

		return holdRemoteOutputs(bw, []string{artifact}, nil, held, holder)
	}

	if resourceType.Config.Backend() == config.BackendWebhook {
		fetch = func(dir string) error { return writeDelivery(ctx, st, resource.Name, version, dir) }
	}

	err := resourceDir(ctx, cfg, artifact, resourceType, resource.HashedEnv(), resource.Source, version, params, bw, fetch)
	if err != nil {
		return fmt.Errorf("could not fetch resource %q: %w", resource.Name, classifyRunError(ctx, err))
	}

	return nil
}

// resourceDir materializes artifact's directory and populates it with the
// given version, either by running the resource type's in: or — when the
// pipeline enabled the cross-build resource cache and this exact version has
// been fetched before — by reusing what an earlier build fetched.
//
// The cache key deliberately is NOT the get node's hash (see
// merkle.ResourceCacheKey): a node hash carries the step's position in a plan,
// so keying on it would give every job its own copy of identical bytes. With
// the cache off, FetchResource is the plain path.
func resourceDir(
	ctx context.Context, cfg *config.Config, artifact string,
	resourceType config.ResourceType, extraEnv []string, source, version, params map[string]any,
	bw workspace.BuildWorkspace, fetch func(dir string) error,
) error {
	// A key this package cannot compute is not a reason to fail the fetch —
	// an empty key simply means "do not cache this one".
	key, err := merkle.ResourceCacheKey(cfg, resourceType, extraEnv, source, version, params)
	if err != nil {
		slog.DebugContext(ctx, "job.get.cache_key_failed", "artifact", artifact, "error", err)

		key = ""
	}

	// Not wrapped with the artifact name: this error is either the fetch's own
	// (which the caller classifies as a task failure via IsExitError) or the
	// workspace's, which already names the directory it failed on.
	_, err = bw.FetchResource(ctx, artifact, key, fetch)

	return err //nolint:wrapcheck // see above: the error is the caller-classified fetch error, passed through deliberately
}
