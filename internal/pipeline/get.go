package pipeline

// get: — resolving a resource's versions and materializing them, either as a
// fan-out of triggered builds or in place inside one.

import (
	"context"
	"errors"
	"fmt"
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

	resource, resourceType, _, err := fetchGetVersions(ctx, w.cfg, step, w.pinned, w.cache)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	sets := w.resolution.sets

	logFrom(ctx).Debug("job.step", "resource", step.Get, "sets", len(sets))

	if len(sets) == 0 {
		w.reportNoVersions(ctx, step, resource.Name, len(remainder))
	}

	var buildErrs []error

	// A pinned run consumes nothing. Naming a version is an instruction
	// outside the every-flow — the consumed filter already exempts pinned
	// runs (Cache.unconsumed), and the recording side has to match, because
	// the cursor is a high-water mark over discovery order: a pin resolved
	// outside history is minted at the TOP order, and taking it would leap
	// the mark over every unbuilt version below it. The set-based cursor
	// recorded pins harmlessly; a mark cannot.
	pinnedRun := len(w.pinned) > 0

	for setIndex, set := range sets {
		// Stop starting NEW triggered builds on cancellation; don't let one
		// abandon itself mid-flight. Mirrors internal/trigger's worker loop.
		if ctx.Err() != nil {
			break
		}

		version := set[step.Get]
		if version == nil {
			return fmt.Errorf("step %d (get %q): the input set binds no version for it", i, step.Get)
		}

		content, err := merkle.GetNodeContent(w.cfg, step, *resourceType, resource.Env, resource.Source, version)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		hash, err := merkle.HashNode(merkle.NodeKindGet, content, w.parentHash)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		if w.skippable[hash] {
			fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
			logFrom(ctx).Info("job.skip", "resource", resource.Name, "reason", "cached", "hash", hash)
			publishStepSkipped(ctx, w.jobName, i, step, hash, skipReason(stepChainSkipped))

			// Taken, even though nothing ran: the cache skipped it because
			// this exact chain already succeeded, which is the definition of
			// a set this job is done with. All of the set's bindings advance,
			// not just this get's — consecutive sets can share a HELD first
			// get's hash, and each skip must still move the other cursors.
			w.takeSet(ctx, pinnedRun, set)

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
		w.takeSet(ctx, pinnedRun, set)

		err = w.runTriggeredBuild(ctx, step, *resource, *resourceType, set, setIndex, remainder, node)

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
func (w *planWalk) reportNoVersions(ctx context.Context, step config.Step, resourceName string, remaining int) {
	// An input that could bind NOTHING — no unconsumed version, no held one —
	// is named, because "no versions" from a sibling's perspective reads as
	// idle when the real story is a resource that has never had a version at
	// all.
	if blocked := w.resolution.blockingReport(); blocked != "" {
		fmt.Printf("get: %s cannot build; no versions exist for: %s\n", resourceName, blocked)
		logFrom(ctx).Warn("job.get.blocked", "resource", resourceName, "blocking", blocked)

		return
	}

	if taken := w.cache.Suppressed(step); taken > 0 {
		fmt.Printf("get: %s has no new versions; all %d already taken\n", resourceName, taken)
		logFrom(ctx).Info("job.get.no_new_versions", "resource", resourceName, "already_taken", taken)

		return
	}

	fmt.Printf("get: %s returned no versions; the %d step(s) after it did not run\n", resourceName, remaining)
	logFrom(ctx).Warn("job.get.no_versions", "resource", resourceName, "skipped_steps", remaining)
}

// takeSet advances the cursor of every fanning get to its binding in this set
// — a set is consumed as a unit, whatever its first get's fate. The binding is
// looked up by GET name and recorded against the RESOURCE, which is where the
// cursor lives. Re-taking a held version is a MAX no-op.
func (w *planWalk) takeSet(ctx context.Context, pinnedRun bool, set merkle.InputSet) {
	if pinnedRun {
		return
	}

	for _, every := range w.resolution.everyInputs {
		if version := set[every.input]; version != nil {
			w.cursor.take(ctx, w.st, w.jobName, every.resource, version)
		}
	}
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
	set merkle.InputSet, setIndex int, remainder []config.Step, node merkle.Node,
) error {
	version := set[step.Get]

	// The versions THIS build fetches, kept apart from its siblings'. A run
	// fans out into one build per input set, and passed: asks whether some
	// one build was green against a combination — so a job-wide record would
	// both correlate versions that never ran together and, being keyed per
	// resource, keep only the last set's. See recordPassedVersions.
	ctx, fetched := withFetchedVersions(ctx)

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

	// Registered here as well as in fetchGetStepInPlace, because a job whose
	// FIRST get is trigger-eligible only ever fetches through this path — and
	// a fetch nobody registers is a green version nobody records, which means
	// a passed: gate downstream of such a job could never open. Latent while
	// the gate was checked only at trigger time against hand-me-down state;
	// loud the moment resolution started reading job_versions for real.
	recordFetchedVersion(ctx, resource.Name, version)

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
	// Every get in the remainder binds this build's set — see
	// fetchGetStepInPlace, which reads it before anything else.
	remainderWalk.assigned = set

	err = runSteps(ctx, remainderWalk, remainder)
	buildOK = err == nil

	// Green is per BUILD, recorded when that build succeeds — Concourse
	// records a build's inputs against the build, and a later set failing
	// says nothing about an earlier one that passed. Waiting for the whole
	// job instead lost every set but the last, and stranded all of them when
	// any one set failed: taken at build start, never green, never retried.
	if buildOK {
		recordPassedVersions(ctx, w.st, w.jobName, buildIDForSet(ctx, setIndex), fetched)
	}

	return err
}

// buildIDForSet names one build of a run, for correlating the versions it
// fetched. Scoped to the run id so two runs never look like one build, and
// numbered within it because sets that HOLD a shared first get otherwise
// produce identical node hashes.
func buildIDForSet(ctx context.Context, setIndex int) string {
	run := ""
	if resume := resumeFrom(ctx); resume != nil {
		run = resume.id
	}

	return fmt.Sprintf("%s#%d", run, setIndex)
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

// resolveInPlaceVersion picks the single version an in-place get fetches: the
// build's input-set binding, else what the step resolves to on its own. A nil
// version (with no error) means the check came back empty and the get fetches
// nothing.
func (w *planWalk) resolveInPlaceVersion(
	ctx context.Context, step config.Step,
) (*config.Resource, *config.ResourceType, map[string]any, error) {
	resource, resourceType, versions, err := fetchGetVersions(ctx, w.cfg, step, w.pinned, w.cache)
	if err != nil {
		return nil, nil, nil, err
	}

	versions, err = w.bindAssigned(step, versions)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("step %d: %w", w.index, err)
	}

	// Same silence, milder consequence than a fan-out's: the rest of the plan
	// still runs, just without the artifact this get was supposed to
	// materialize, so a later step fails on a missing input instead of on the
	// empty check that caused it. Name the cause here, where it is known.
	if len(versions) == 0 {
		fmt.Printf("get: %s returned no versions; nothing was fetched\n", step.Get)
		logFrom(ctx).Warn("job.get.no_versions", "resource", step.Get)

		return resource, resourceType, nil, nil
	}

	// Inside a triggered build a get resolves to a single version.
	return resource, resourceType, versions[0], nil
}

// fetchGetStepInPlace resolves one version and fetches it into the walk's
// current workspace, returning the new parentHash — or stepChainSkipped when
// the node's hash is already in the skippable index.
func (w *planWalk) fetchGetStepInPlace(ctx context.Context, step config.Step) (stepResult, error) {
	i := w.index

	resource, resourceType, version, err := w.resolveInPlaceVersion(ctx, step)
	if err != nil {
		return stepResult{}, err
	}

	if version == nil {
		return ran(w.parentHash), nil
	}

	recordFetchedVersion(ctx, resource.Name, version)

	content, err := merkle.GetNodeContent(w.cfg, step, *resourceType, resource.Env, resource.Source, version)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindGet, content, w.parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	if w.skippable[hash] {
		fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
		logFrom(ctx).Info("job.skip", "resource", resource.Name, "reason", "cached", "hash", hash)

		return stepResult{hash: w.parentHash, disposition: stepChainSkipped}, nil
	}

	node := merkle.Node{Hash: hash, ParentHash: w.parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

	// Recorded on the same terms as the fan-out path (see runTriggeredBuild):
	// once the step is known to run, BEFORE the fetch and its hooks. A get
	// that fetched appears in assert.execution under its resource's name, and
	// with input sets a later get is a full participant in the fan-out rather
	// than a footnote of the first. Recording it afterwards put a get behind
	// its own hooks, inverting the [step, its hooks...] order every other
	// step kind keeps, and hid a get whose fetch failed.
	recordExecution(ctx, resource.Name)

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

// bindAssigned returns the build's input-set binding for a get, as the single
// version to fetch. It is consulted BEFORE fetchGetStepInPlace's empty check,
// which is load-bearing: an every-get HELD at an already-consumed version has
// an empty consumed-filtered cache entry, and without the binding the build
// would silently lack its artifact.
//
// Keyed by the get's own name, so a get aliasing a resource another get fans
// over keeps the version IT resolved. An unbound get is an error rather than a
// fallback: the planner hashed the set's binding, so choosing a different
// version here would have the cache record that chain as done for work it
// never did.
func (w *planWalk) bindAssigned(step config.Step, versions []map[string]any) ([]map[string]any, error) {
	if w.assigned == nil {
		return versions, nil
	}

	assigned := w.assigned[step.Get]
	if assigned == nil {
		return nil, fmt.Errorf("get %q: the input set binds no version for it", step.Get)
	}

	return []map[string]any{assigned}, nil
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
		logFrom(ctx).Info("job.get.attempt", "get", step.Get, "attempt", attempt, "total_attempts", total)
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
		logFrom(ctx).Info("job.get.in.attempt", "artifact", artifact, "attempt", attempt, "total_attempts", total)
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

	err := resourceDir(ctx, cfg, artifact, resourceType, resource.Env, resource.Source, version, params, bw, func(dir string) error {
		return rsrc.RunIn(ctx, cfg, resourceType, resource.Env, resource.Source, version, params, dir)
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
	resourceType config.ResourceType, extraEnv []string, source, version, params map[string]any,
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
	key, err := merkle.ResourceCacheKey(cfg, resourceType, extraEnv, source, version, params)
	if err != nil {
		logFrom(ctx).Debug("job.get.cache_key_failed", "artifact", artifact, "error", err)

		key = ""
	}

	// Not wrapped with the artifact name: this error is either the fetch's own
	// (which the caller classifies as a task failure via IsExitError) or the
	// workspace's, which already names the directory it failed on.
	_, err = caching.FetchResource(ctx, artifact, key, fetch)

	return err //nolint:wrapcheck // see above: the error is the caller-classified fetch error, passed through deliberately
}
