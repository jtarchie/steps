package pipeline

// get: — resolving a resource's versions and materializing them, either as a
// fan-out of triggered builds or in place inside one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/workspace"
)

// fanOutGet resolves and (unless skippable) fetches step's resource
// version(s), then runs the remainder of the plan for each — see
// runTriggeredBuild. It always terminates the calling walk, since a get step
// delegates the rest of the plan to its triggered build(s).
//
// A version whose triggered build fails does NOT stop the remaining versions
// from being attempted (see TestConformanceGetVersionEveryContinuesPastFailure):
// Concourse's own version-selection cursor (atc/db/versions_db.go's
// NextEveryVersion) advances regardless of a prior build's status, and every
// version here already gets its own isolated workspace/hooks/store-recording.
// Structural errors (bad template, unmarshalable version) are the one
// exception: those depend only on static step/version content, so they recur
// identically for every version and aborting immediately is still right.
func (w *planWalk) fanOutGet(ctx context.Context, step config.Step, remainder []config.Step) error {
	i := w.index

	resource, resourceType, versions, err := fetchGetVersions(ctx, w.cfg, step, w.pinned, w.cache)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	slog.Debug("job.step", "job", w.jobName, "index", i, "kind", "get", "resource", step.Get, "versions", len(versions))

	if len(versions) == 0 {
		w.reportNoVersions(step, resource.Name, len(remainder))
	}

	var buildErrs []error

	for _, version := range versions {
		// Stop starting NEW triggered builds on cancellation; don't let one
		// abandon itself mid-flight. Mirrors internal/trigger's worker loop.
		if ctx.Err() != nil {
			break
		}

		content, err := merkle.GetNodeContent(w.cfg, step, *resourceType, resource.Source, version)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		hash, err := merkle.HashNode(merkle.NodeKindGet, content, w.parentHash)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		if w.skippable[hash] {
			fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
			slog.Info("job.skip", "job", w.jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)
			publishStepSkipped(ctx, w.jobName, i, step, hash, skipReason(stepChainSkipped))

			// Taken, even though nothing ran: the cache skipped it because
			// this exact chain already succeeded, which is the definition of
			// a version this job is done with.
			w.cursor.take(ctx, w.st, w.jobName, resource.Name, version, w.cfg.VersionHistoryLimit())

			continue
		}

		node := merkle.Node{Hash: hash, ParentHash: w.parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		// A fan-out get publishes per VERSION: each one triggers its own build
		// of the remaining plan, so each is its own start/finish pair rather
		// than one event for the step as a whole.
		getStarted := time.Now()

		publishStepStarted(ctx, w.jobName, i, step)

		// Taken BEFORE the build, not after it succeeds — Concourse's own
		// rule. NextEveryVersion reads build_resource_config_version_inputs, a
		// table of the versions a build was CREATED with, with no filter on
		// build status: a version consumed by a failed build is consumed, and
		// the cursor moves on. Re-running one is an explicit act there
		// (concourse/concourse#413), which here is --force or --resume.
		//
		// The tempting alternative — take it only on success, so a failure is
		// retried — was tried and reverted. It makes a version that fails
		// forever re-run forever, on every trigger, with an agent's bill
		// attached, and it means "every version, once" quietly is not true.
		w.cursor.take(ctx, w.st, w.jobName, resource.Name, version, w.cfg.VersionHistoryLimit())

		err = w.runTriggeredBuild(ctx, step, *resource, *resourceType, version, remainder, node)

		publishStepFinished(ctx, w.jobName, i, step, hash, getStarted, err)

		if err != nil {
			buildErrs = append(buildErrs, fmt.Errorf("step %d (get %q) version %v: %w", i, step.Get, version, err))
		}
	}

	return errors.Join(buildErrs...)
}

// reportNoVersions explains a get that selected nothing.
//
// version:every is the ONLY path that can reach here empty: every other mode
// narrows to a pin, and SelectVersion errors on an empty check. So the plan
// runs zero builds and the job "succeeds", outwardly identical to one whose
// steps all ran. "Nothing new upstream" is idle and "the check is broken or
// its source is gone" is not, so say which — and warn only on the second, or
// the alarm becomes noise on every poll of a watched resource.
func (w *planWalk) reportNoVersions(step config.Step, resourceName string, remaining int) {
	if taken := w.cache.Suppressed(step); taken > 0 {
		fmt.Printf("get: %s has no new versions; all %d already taken\n", resourceName, taken)
		slog.Info("job.get.no_new_versions", "job", w.jobName, "index", w.index, "resource", resourceName, "already_taken", taken)

		return
	}

	fmt.Printf("get: %s returned no versions; the %d step(s) after it did not run\n", resourceName, remaining)
	slog.Warn("job.get.no_versions", "job", w.jobName, "index", w.index, "resource", resourceName, "skipped_steps", remaining)
}

// runTriggeredBuild runs the build that a single resource version triggers:
// per Concourse's model, the version triggering a get is what starts a build,
// and every build gets its own isolated working directory. So this creates a
// fresh workspace for just this version, fetches the version into it, runs the
// remainder of the plan inside it, and tears the workspace down afterward —
// never sharing it with any other triggered build, including sibling versions
// fanned out by version:every.
func (w *planWalk) runTriggeredBuild(
	ctx context.Context, step config.Step, resource config.Resource, resourceType config.ResourceType,
	version map[string]any, remainder []config.Step, node merkle.Node,
) error {
	bw, err := w.provider.NewBuild(ctx, resource.Name)
	if err != nil {
		return fmt.Errorf("could not create workspace for %q: %w", resource.Name, err)
	}

	// Torn down only when the build SUCCEEDS. A failed build's tree is what a
	// resume continues in — the same reason RunJob keeps the workspace on
	// failure — and destroying it here is what made an isolated run
	// unresumable in practice: the run row pointed at a deleted directory.
	buildOK := false

	defer func() {
		if buildOK {
			workspace.CloseBuild(bw, resource.Name)
		}
	}()

	build := w.withBuild(bw)

	// Re-point the run at THIS build. A get fans the rest of the plan out per
	// version into a build of its own, and that is where the artifacts and
	// every subsequent step live — the job-level build recorded at RunJob
	// holds none of it. StartRun upserts, so this is the same row with a
	// better answer.
	if rooted, ok := bw.(workspace.RootedBuild); ok {
		if resume := resumeFrom(ctx); resume != nil {
			_ = w.st.StartRun(ctx, resume.id, w.jobName, rooted.Root())
		}
	}

	recordExecution(ctx, resource.Name)

	err = fetchGetStepWithStep(ctx, w.cfg, step, step.Get, resource, resourceType, version, bw)

	// Get-step hooks fire once per triggered build, in that build's own
	// workspace, observing the fetch outcome. A fetch failure (or a hook that
	// fails an otherwise-green fetch) fails this build.
	if !step.Hooks.Empty() {
		err = runHooks(ctx, build.scope(stepLabel(w.index, step)), step.Hooks, err)
	}

	if err != nil {
		recordStepFailure(ctx, build, node, err)

		return err
	}

	err = w.st.RecordNode(ctx, nodeRecord(node), w.jobName, "succeeded", nil, nil)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	remainderWalk := *w
	remainderWalk.stepRunner = build
	remainderWalk.parentHash = node.Hash
	remainderWalk.allowGetTrigger = false

	err = runSteps(ctx, remainderWalk, remainder)
	buildOK = err == nil

	return err
}

// fetchInPlace fetches one version of a get step's resource into the existing
// build workspace rather than creating a new triggered build — the path taken
// inside a triggered build's remainder, where consecutive gets share a
// workspace. It advances the walk, and returns done=true when the chain was
// skipped (nil error) or the fetch failed.
func (w *planWalk) fetchInPlace(ctx context.Context, step config.Step, steps []config.Step) (bool, error) {
	started := time.Now()

	publishStepStarted(ctx, w.jobName, w.index, step)

	res, err := w.fetchGetStepInPlace(ctx, step)
	if err != nil {
		publishStepFinished(ctx, w.jobName, w.index, step, res.hash, started, err)

		return true, err
	}

	if res.disposition == stepChainSkipped {
		publishStepSkipped(ctx, w.jobName, w.index, step, res.hash, skipReason(res.disposition))
		reportChainSkipped(ctx, w.jobName, w.index+1, steps[w.index+1:])

		return true, nil
	}

	publishStepFinished(ctx, w.jobName, w.index, step, res.hash, started, nil)

	if res.hash != "" {
		w.parentHash = res.hash
	}

	w.index++

	return false, nil
}

// fetchGetStepInPlace resolves one version and fetches it into the walk's
// current workspace, returning the new parentHash — or stepChainSkipped when
// the node's hash is already in the skippable index.
func (w *planWalk) fetchGetStepInPlace(ctx context.Context, step config.Step) (stepResult, error) {
	i := w.index

	resource, resourceType, versions, err := fetchGetVersions(ctx, w.cfg, step, w.pinned, w.cache)
	if err != nil {
		return stepResult{}, err
	}

	// Same silence, milder consequence than a fan-out's: the rest of the plan
	// still runs, just without the artifact this get was supposed to
	// materialize, so a later step fails on a missing input instead of on the
	// empty check that caused it. Name the cause here, where it is known.
	if len(versions) == 0 {
		fmt.Printf("get: %s returned no versions; nothing was fetched\n", step.Get)
		slog.Warn("job.get.no_versions", "job", w.jobName, "index", i, "resource", step.Get)

		return ran(w.parentHash), nil
	}

	// Inside a triggered build a get resolves to a single version.
	version := versions[0]

	recordFetchedVersion(ctx, resource.Name, version)

	content, err := merkle.GetNodeContent(w.cfg, step, *resourceType, resource.Source, version)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindGet, content, w.parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	if w.skippable[hash] {
		fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
		slog.Info("job.skip", "job", w.jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)

		return stepResult{hash: w.parentHash, disposition: stepChainSkipped}, nil
	}

	node := merkle.Node{Hash: hash, ParentHash: w.parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

	err = fetchGetStepWithStep(ctx, w.cfg, step, step.Get, *resource, *resourceType, version, w.bw)

	// Get-step hooks fire in the same workspace the resource was fetched into.
	if err == nil && !step.Hooks.Empty() {
		err = runHooks(ctx, w.scope(stepLabel(i, step)), step.Hooks, err)
	}

	if err != nil {
		recordStepFailure(ctx, w.stepRunner, node, err)

		return stepResult{}, err
	}

	err = w.st.RecordNode(ctx, nodeRecord(node), w.jobName, "succeeded", nil, nil)
	if err != nil {
		return stepResult{}, fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return ran(hash), nil
}

// fetchGetVersions resolves a get step's versions with retries and timeout
// support, returning the resource, its type, and the versions to fetch.
func fetchGetVersions(ctx context.Context, cfg *config.Config, step config.Step, pinned map[string]string, cache *rsrc.Cache) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	var (
		resource     *config.Resource
		resourceType *config.ResourceType
		versions     []map[string]any
	)

	err := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		fmt.Printf("get: %s (attempt %d/%d)\n", step.Get, attempt, total)
		slog.Info("job.get.attempt", "get", step.Get, "attempt", attempt, "total_attempts", total)
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
func fetchGetStepWithStep(ctx context.Context, cfg *config.Config, step config.Step, artifact string, resource config.Resource, resourceType config.ResourceType, version map[string]any, bw workspace.BuildWorkspace) error {
	err := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		fmt.Printf("get: %s (version: %v, attempt %d/%d)\n", artifact, version, attempt, total)
		slog.Info("job.get.in.attempt", "artifact", artifact, "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		return fetchGetStep(attemptCtx, cfg, artifact, resource, resourceType, version, step.Params, bw)
	})
	if err != nil {
		return fmt.Errorf("get %q: %w", artifact, err)
	}

	return nil
}

func fetchGetStep(ctx context.Context, cfg *config.Config, artifact string, resource config.Resource, resourceType config.ResourceType, version, params map[string]any, bw workspace.BuildWorkspace) error {
	fmt.Printf("get: %s (version: %v)\n", artifact, version)

	err := resourceDir(ctx, cfg, artifact, resourceType, resource.Source, version, params, bw, func(dir string) error {
		return rsrc.RunIn(ctx, cfg, resourceType, resource.Source, version, params, dir)
	})
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
// so keying on it would give every job its own copy of identical bytes. A
// build workspace that cannot cache (the default shared one, or an isolating
// one with the cache off) takes the plain path.
func resourceDir(
	ctx context.Context, cfg *config.Config, artifact string,
	resourceType config.ResourceType, source, version, params map[string]any,
	bw workspace.BuildWorkspace, fetch func(dir string) error,
) error {
	caching, ok := bw.(workspace.CachingBuild)
	if !ok {
		dir, err := bw.ResourceDir(ctx, artifact)
		if err != nil {
			return fmt.Errorf("could not create resource dir for %q: %w", artifact, err)
		}

		return fetch(dir)
	}

	// A key this package cannot compute is not a reason to fail the fetch —
	// an empty key simply means "do not cache this one".
	key, err := merkle.ResourceCacheKey(cfg, resourceType, source, version, params)
	if err != nil {
		slog.Debug("job.get.cache_key_failed", "artifact", artifact, "error", err)

		key = ""
	}

	// Not wrapped with the artifact name: this error is either the fetch's own
	// (which the caller classifies as a task failure via IsExitError) or the
	// workspace's, which already names the directory it failed on.
	_, err = caching.FetchResource(ctx, artifact, key, fetch)

	return err //nolint:wrapcheck // see above: the error is the caller-classified fetch error, passed through deliberately
}
