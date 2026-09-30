package pipeline

// get: — resolving a resource's versions and materializing them, either as a
// fan-out of triggered builds or in place inside one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/venue"
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

	// getCtx carries this get's own log identity, and deliberately does NOT
	// reach runTriggeredBuild below: the remainder of the plan is other
	// steps, each of which stamps its own, and handing them this one would
	// have every later line claim to be the get's.
	getCtx := withStepLogger(ctx, i, step)

	resource, resourceType, _, err := fetchGetVersions(getCtx, w.cfg, step, w.pinned, w.cache)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	sets := w.resolution.sets

	slog.DebugContext(getCtx, "job.step", "resource", step.GetResourceName(), "sets", len(sets))

	if len(sets) == 0 {
		w.reportNoVersions(getCtx, step, resource.Name, len(remainder))
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
			slog.InfoContext(getCtx, "job.skip", "resource", resource.Name, "reason", "cached", "hash", hash)

			skipMark := markStep(getCtx)
			publishStepSkipped(getCtx, w.jobName, i, step, skipMark, hash, skipReason(stepChainSkipped))

			// Nested and numbered as this set's triggered build would have
			// published them, so the most common fully-cached run — one whose
			// plan opens with a get — still names every step it replayed.
			w.reportChainSkipped(withChildrenOf(ctx, skipMark), hash, 0, remainder)

			// A skip means this exact chain already succeeded once — the version
			// was genuinely fetched, just not by this run. Mirrors
			// fetchGetStepInPlace's own skip branch: without this, a job whose
			// FIRST get is unpolled goes stale in resource_checks the moment its
			// chain starts being cached, because this is the only skip path for
			// that get and nothing else ever calls recordResolvedVersion again.
			recordResolvedVersion(ctx, w.st, w.cfg, resource.Name, version, pinnedRun)

			// Taken, even though nothing ran: the cache skipped it because
			// this exact chain already succeeded, which is the definition of
			// a set this job is done with. All of the set's bindings advance,
			// not just this get's — consecutive sets can share a HELD first
			// get's hash, and each skip must still move the other cursors.
			w.takeSet(ctx, pinnedRun, set, setIndex)

			continue
		}

		node := merkle.Node{Hash: hash, ParentHash: w.parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		// A fan-out get publishes per VERSION: each one triggers its own build
		// of the remaining plan, so each is its own start/finish pair rather
		// than one event for the step as a whole.
		getStarted := time.Now()

		mark := publishStepStarted(getCtx, w.jobName, i, step)

		// Taken BEFORE the build, not after it succeeds — Concourse's own
		// rule. NextEveryVersion reads build_resource_config_version_inputs, a
		// table of the versions a build was CREATED with, with no filter on
		// build status: a version consumed by a failed build is consumed, and
		// the cursor moves on. Re-running one is an explicit act there
		// (concourse/concourse#413), which here is --resume (that build) or --pin (that version).
		//
		// The tempting alternative — take it only on success, so a failure is
		// retried — was tried and reverted. It makes a version that fails
		// forever re-run forever, on every trigger, with an agent's bill
		// attached, and it means "every version, once" quietly is not true.
		w.takeSet(ctx, pinnedRun, set, setIndex)

		// The get is the container of everything the version it selected goes
		// on to build — which is what it already IS, since runTriggeredBuild
		// runs the whole remainder of the plan and this step does not finish
		// until that does. Only the tree was missing.
		err = w.runTriggeredBuild(withChildrenOf(ctx, mark), step, *resource, *resourceType, set, setIndex, remainder, node)

		publishStepFinished(getCtx, w.jobName, i, step, mark, hash, getStarted, err)

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
		warnf(ctx, "get: %s cannot build; no versions exist for: %s", resourceName, blocked)
		slog.WarnContext(ctx, "job.get.blocked", "resource", resourceName, "blocking", blocked)

		return
	}

	if taken := w.cache.Suppressed(step); taken > 0 {
		reason := fmt.Sprintf("no new versions; all %d already taken", taken)
		if forced(ctx) {
			reason += " — --force skips the step cache, not versions this job already took; to redo one, resume the run that took it or pin the version"
		}

		slog.InfoContext(ctx, "job.get.no_new_versions", "resource", resourceName, "already_taken", taken, "forced", forced(ctx))
		publishStepSkipped(ctx, w.jobName, w.index, step, markStep(ctx), "", reason)

		return
	}

	warnf(ctx, "get: %s returned no versions; the %d step(s) after it did not run", resourceName, remaining)
	slog.WarnContext(ctx, "job.get.no_versions", "resource", resourceName, "skipped_steps", remaining)
}

// takeSet advances the cursor of every fanning get to its binding in this set
// — a set is consumed as a unit, whatever its first get's fate — and records
// what the build was created with: EVERY get's version, fixed gets and pins
// included, which is what a resume rebuilds it against (fly rerun-build's
// build_resource_config_version_inputs). The binding is looked up by GET name
// and the cursor advanced by RESOURCE, which is where it lives.
//
// A resumed build records nothing and takes nothing: the run it continues did
// both when it created the build. A pinned run records and does not take —
// naming a version is an instruction outside the every-flow (the consumed
// filter already exempts pinned runs, Cache.unconsumed), and the cursor is a
// high-water mark over discovery order, so a pin resolved outside history is
// minted at the TOP order and taking it would leap the mark over every
// unbuilt version below it. Re-taking a held version is a MAX no-op.
func (w *planWalk) takeSet(ctx context.Context, pinnedRun bool, set merkle.InputSet, setIndex int) {
	if w.resolution.recorded {
		return
	}

	buildID := buildIDForSet(ctx, setIndex)

	for input, version := range set {
		w.recordRunInput(ctx, buildID, input, w.resolution.resources[input], version)
	}

	if pinnedRun || w.resolution.rerun {
		return
	}

	for _, every := range w.resolution.everyInputs {
		if version := set[every.input]; version != nil {
			w.cursor.take(ctx, w.st, w.jobName, every.resource, version)
		}
	}
}

// recordRunInput files the version this build was created with, so --resume
// can reach it after the cursor has moved past it. The cursor mark beside it
// says the version is spent; this says which run spent it, which is the half
// a recovery needs.
//
// Best-effort, and detached for the same reason take is: a build already under
// way must not fail over bookkeeping, and the cost of a lost row is a resume
// that cannot re-open one version — which is where this path started, so it is
// no worse than not recording at all.
func (w *planWalk) recordRunInput(ctx context.Context, buildID, inputName, resourceName string, version map[string]any) {
	key, ok := encodeVersion(version)
	if !ok {
		return
	}

	err := w.st.RecordRunInput(context.WithoutCancel(ctx), events.RunID(ctx), buildID, inputName, resourceName, key)
	if err != nil {
		slog.WarnContext(ctx, "job.run_input_unrecorded", "get", inputName, "resource", resourceName, "error", err)
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
	runCtx := ctx
	ctx, fetched := withBuildVersions(ctx)

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
	// holds none of it. An UPDATE of the row RunJob already wrote, never a
	// mint: this is the same run with a better answer about where it lives.
	root := ""
	if rooted, ok := bw.(workspace.RootedBuild); ok {
		root = rooted.Root()
	}

	w.pointRunAt(ctx, root)

	recordExecution(ctx, resource.Name)

	// Registered here as well as in fetchGetStepInPlace, because a job whose
	// FIRST get is trigger-eligible only ever fetches through this path — and
	// a fetch nobody registers is a green version nobody records, which means
	// a passed: gate downstream of such a job could never open. Latent while
	// the gate was checked only at trigger time against hand-me-down state;
	// loud the moment resolution started reading job_versions for real.
	recordBuildVersion(ctx, resource.Name, version)

	buildID := buildIDForSet(ctx, setIndex)

	if runID, kept := keptFetch(ctx, buildID, -1); kept {
		// The get stays the container of its build, so it keeps its
		// started/finished pair and says why it fetched nothing on its row.
		err = w.keepFetched(events.WithStepID(ctx, parentStepFrom(ctx)), runID, buildID, step, resource.Name, version, bw)
		if err != nil {
			return err
		}
	} else {
		err = w.fetchTriggered(ctx, build, bw, step, resource, resourceType, version, node)
		if err != nil {
			return err
		}
	}

	remainderWalk := *w
	remainderWalk.stepRunner = build
	remainderWalk.build = buildID
	remainderWalk.parentHash = node.Hash
	remainderWalk.allowGetTrigger = false
	// Every get in the remainder binds this build's set — see
	// fetchGetStepInPlace, which reads it before anything else.
	remainderWalk.assigned = set

	err = runSteps(ctx, remainderWalk, remainder)
	buildOK = err == nil

	// A green build's tree is removed as this returns, so the row goes back
	// to the last build that failed: left naming the deleted tree, a resume
	// of an earlier failure behind a later success had nothing to continue.
	if buildOK {
		w.pointRunAt(context.WithoutCancel(ctx), w.failedRoot)
	} else {
		w.failedRoot = root
	}

	// Green is per BUILD, recorded when that build succeeds — Concourse
	// records a build's inputs against the build, and a later set failing
	// says nothing about an earlier one that passed. Waiting for the whole
	// job instead lost every set but the last, and stranded all of them when
	// any one set failed: taken at build start, never green, never retried.
	if buildOK {
		recordPassedVersions(ctx, w.st, w.jobName, buildID, fetched)
		noteGreenBuild(runCtx, buildID)
	}

	return err
}

// pointRunAt re-points the run's row at a build's tree; "" leaves it alone.
func (w *planWalk) pointRunAt(ctx context.Context, root string) {
	resume := resumeFrom(ctx)
	if resume == nil || root == "" {
		return
	}

	// The same configuration the run is already recorded under: this is a
	// workspace correction, not a change of what is executing.
	err := w.st.ResumeRun(ctx, resume.id, root, w.cfg.Revision.SHA)
	if err != nil {
		// Logged, not returned: the row exists by the time a get runs, so
		// this cannot fail for a reason the get can act on, and a version
		// fetched is not made wrong by a stale workspace column.
		slog.WarnContext(ctx, "job.run_workspace_unrecorded", "run", resume.id, "error", err)
	}
}

// fetchTriggered fetches a triggered build's first get into its workspace,
// with the get's hooks.
func (w *planWalk) fetchTriggered(
	ctx context.Context, build stepRunner, bw workspace.BuildWorkspace, step config.Step,
	resource config.Resource, resourceType config.ResourceType, version map[string]any, node merkle.Node,
) error {
	_, err := runPlaced(ctx, build, node, step.Get, noResult(func(placedCtx context.Context) error {
		// Notes and log lines about the fetch are the get's, and the context here is its CHILDREN'S (ctx holds the get as their parent), so name the get itself: recorded against no step, the version it fetched was drawn apart from its row.
		fetchCtx := withStepLogger(events.WithStepID(placedCtx, parentStepFrom(ctx)), w.index, step)

		err := fetchGetStepWithStep(fetchCtx, w.cfg, w.st, step, step.Get, resource, resourceType, version, bw)

		// Get-step hooks fire once per triggered build, in that build's own
		// workspace, observing the fetch outcome. A fetch failure (or a hook that
		// fails an otherwise-green fetch) fails this build.
		if !step.Hooks.Empty() {
			err = runHooks(ctx, build.scope(stepLabel(w.index, step)), step.Hooks, err)
		}

		if err != nil {
			return err
		}

		// Only now that the fetch (and its hooks) actually succeeded: recording
		// it earlier would show resource_checks a version nothing ever fetched.
		recordResolvedVersion(ctx, w.st, w.cfg, resource.Name, version, len(w.pinned) > 0)

		return nil
	}))

	return err
}

// keptFetch reports whether a resumed get must keep what an earlier attempt
// of its run fetched: a later step of its build completed, so the fetch
// succeeded and that step may have changed the artifact — fetching again
// would replace the work the resume is about to skip.
func keptFetch(ctx context.Context, build string, index int) (string, bool) {
	resume := resumeFrom(ctx)
	if resume == nil || resume.refetch || !resume.progressedPast(build, index) {
		return "", false
	}

	return resume.id, true
}

// keepInPlace is keptFetch and keepFetched for an in-place get, reporting
// whether it kept; a kept get chains the plan on under its own node.
func (w *planWalk) keepInPlace(
	ctx context.Context, step config.Step, resourceName string, version map[string]any, hash string,
) (stepResult, bool, error) {
	runID, kept := keptFetch(ctx, w.build, w.index)
	if !kept {
		return stepResult{}, false, nil
	}

	err := w.keepFetched(ctx, runID, w.build, step, resourceName, version, w.bw)
	if err != nil {
		return stepResult{}, true, err
	}

	return stepResult{hash: hash, nodeHash: hash}, true, nil
}

// keepFetched stands in for a fetch keptFetch ruled out: it records what the
// fetch would have, so a put's version() and resource_checks read the same,
// and refuses a tree that lost the artifact rather than fetching over the
// steps' work or continuing without it. No node is recorded (the earlier
// attempt did) and no hooks fire, as for any skip. ctx names the get's own
// step, which the skip line is said on.
func (w *planWalk) keepFetched(
	ctx context.Context, runID, build string, step config.Step, resourceName string, version map[string]any, bw workspace.BuildWorkspace,
) error {
	// A finished build is exempt: the kept tree is the last unfinished build's,
	// which need not hold a get it never reached, and nothing of a finished
	// build runs to read it.
	checker, ok := bw.(workspace.ArtifactChecker)
	if ok && !checker.HasArtifact(step.Get) && !resumeFrom(ctx).buildFinished(build) {
		root := ""
		if rooted, ok := bw.(workspace.RootedBuild); ok {
			root = rooted.Root()
		}

		return fmt.Errorf(
			"cannot resume run %q: get %q was already fetched and changed by later steps, but artifacts/%s is not in the kept workspace %s — start a new run, with --pin <field>=<value> to rebuild a version the cursor already took",
			runID, step.Get, step.Get, root)
	}

	notef(ctx, "skip: %s (already fetched)%s", step.Get, buildSuffix(runID, build))
	slog.InfoContext(ctx, "job.skip", "get", step.Get, "reason", "resume")

	recordFetched(ctx, step.Get, version)
	recordResolvedVersion(ctx, w.st, w.cfg, resourceName, version, len(w.pinned) > 0)

	return nil
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

	// Safe to stamp for the whole function, unlike fanOutGet's: this path
	// runs no remainder. It advances the walk and returns, and the loop
	// carries on with its own context.
	ctx = withStepLogger(ctx, w.index, step)

	mark := publishStepStarted(ctx, w.jobName, w.index, step)

	res, err := w.fetchGetStepInPlace(withChildrenOf(events.WithStepID(ctx, mark.id), mark), step)
	if err != nil {
		publishStepFinished(ctx, w.jobName, w.index, step, mark, res.published(), started, err)

		return true, err
	}

	if res.disposition == stepChainSkipped {
		publishStepSkipped(ctx, w.jobName, w.index, step, mark, res.published(), skipReason(res.disposition))
		w.reportChainSkipped(ctx, res.nodeHash, w.index+1, steps[w.index+1:])

		return true, nil
	}

	publishStepFinished(ctx, w.jobName, w.index, step, mark, res.published(), started, nil)

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
		warnf(ctx, "get: %s returned no versions; nothing was fetched", step.Get)
		slog.WarnContext(ctx, "job.get.no_versions", "resource", step.GetResourceName())

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
		return stepResult{hash: w.parentHash}, nil
	}

	recordBuildVersion(ctx, resource.Name, version)

	content, err := merkle.GetNodeContent(w.cfg, step, *resourceType, resource.Env, resource.Source, version)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindGet, content, w.parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	if w.skippable[hash] {
		slog.InfoContext(ctx, "job.skip", "resource", resource.Name, "reason", "cached", "hash", hash)

		// A skip means this exact chain already succeeded once — the version
		// was genuinely fetched, just not by this run.
		recordResolvedVersion(ctx, w.st, w.cfg, resource.Name, version, len(w.pinned) > 0)

		return stepResult{hash: w.parentHash, nodeHash: hash, disposition: stepChainSkipped}, nil
	}

	// Recorded on the same terms as the fan-out path (see runTriggeredBuild):
	// once the step is known to run, BEFORE the fetch and its hooks. A get
	// that fetched appears in assert.execution under its resource's name, and
	// with input sets a later get is a full participant in the fan-out rather
	// than a footnote of the first. Recording it afterwards put a get behind
	// its own hooks, inverting the [step, its hooks...] order every other
	// step kind keeps, and hid a get whose fetch failed.
	recordExecution(ctx, resource.Name)

	if res, kept, err := w.keepInPlace(ctx, step, resource.Name, version, hash); kept {
		return res, err
	}

	node := merkle.Node{Hash: hash, ParentHash: w.parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

	return runPlaced(ctx, w.stepRunner, node, step.Get, noResult(func(ctx context.Context) error {
		err := fetchGetStepWithStep(ctx, w.cfg, w.st, step, step.Get, *resource, *resourceType, version, w.bw)

		// Get-step hooks fire in the same workspace the resource was fetched into.
		if err == nil && !step.Hooks.Empty() {
			err = runHooks(ctx, w.scope(stepLabel(i, step)), step.Hooks, err)
		}

		if err != nil {
			return err
		}

		// Only now that the fetch (and its hooks) actually succeeded: recording
		// it earlier would show resource_checks a version nothing ever fetched.
		recordResolvedVersion(ctx, w.st, w.cfg, resource.Name, version, len(w.pinned) > 0)

		return nil
	}))
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
	recordFetched(ctx, artifact, version)

	return nil
}

func fetchGetStep(ctx context.Context, cfg *config.Config, st store.Deliveries, artifact string, resource config.Resource, resourceType config.ResourceType, version, params map[string]any, bw workspace.BuildWorkspace) error {
	notef(ctx, "get: %s (version: %s)", artifact, versionText(version))

	fetch := func(dir string) error {
		err := rsrc.RunIn(ctx, cfg, resourceType, resource.Env, resource.Source, version, params, dir)
		if err != nil {
			return err //nolint:wrapcheck // the caller classifies and names the resource
		}

		// The stage closed its own runner, so what the worker kept comes out
		// through the sink runPlacedStage installed; recorded inside the
		// fetch, before the resource cache looks at the directory it filled.
		held, holder := heldFrom(ctx)

		return holdRemoteOutputs(ctx, bw, []string{artifact}, nil, held, holder)
	}

	if resourceType.Config.Backend() == config.BackendWebhook {
		fetch = func(dir string) error { return writeDelivery(ctx, st, resource.Name, version, dir) }
	}

	err := resourceDir(ctx, cfg, artifact, resourceType, resource.Env, resource.Source, version, params, bw, fetch)
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
		slog.DebugContext(ctx, "job.get.cache_key_failed", "artifact", artifact, "error", err)

		key = ""
	}

	// Not wrapped with the artifact name: this error is either the fetch's own
	// (which the caller classifies as a task failure via IsExitError) or the
	// workspace's, which already names the directory it failed on.
	_, err = caching.FetchResource(ctx, artifact, key, fetch)

	return err //nolint:wrapcheck // see above: the error is the caller-classified fetch error, passed through deliberately
}

// writeDelivery is a webhook get: the delivery recorded with this version, as body, headers.json and version.json.
func writeDelivery(ctx context.Context, st store.Deliveries, name string, version map[string]any, dir string) error {
	encoded, err := store.EncodeVersion(version)
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	delivery, found, err := st.Delivery(ctx, name, encoded)
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	if !found {
		return fmt.Errorf("get %q: no delivery is recorded for version %s — version_history: may have pruned it", name, encoded)
	}

	headers, err := json.MarshalIndent(delivery.Headers, "", "  ")
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	files := map[string][]byte{"body": delivery.Body, "headers.json": headers, "version.json": []byte(encoded)}

	for file, contents := range files {
		err = os.WriteFile(filepath.Join(dir, file), contents, 0o600)
		if err != nil {
			return fmt.Errorf("get %q: %w", name, err)
		}
	}

	return nil
}

// versionText is a version as a reader sees it everywhere else — the JSON a check emits — rather than Go's map[k:v].
func versionText(version map[string]any) string {
	if key, ok := encodeVersion(version); ok {
		return key
	}

	return fmt.Sprint(version)
}
