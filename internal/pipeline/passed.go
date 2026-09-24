package pipeline

// The passed: constraint's run-time half: remembering which versions a job has
// actually been green against.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// buildVersions is what a job run fetched AND published, per resource: a
// build's inputs and outputs, which is what Concourse's
// successful_build_outputs holds. A job that succeeds records these as
// "passed", which is the fact a downstream job's passed: constraint reads.
// A resource can have several (a get and a put, or repeated puts), so each
// maps to a set.
type buildVersions struct {
	mu sync.Mutex
	by map[string]map[string]bool
	// lastGreen is the id of the last triggered build that went green inside
	// this run. What the run itself records (a job hook's put, a put before
	// the first get) goes under it: recorded under the bare run id, it shared
	// no build with the gets and a downstream fan-in over both never opened.
	// One build, not all: job_versions holds one build per version.
	lastGreen string
}

type buildVersionsKey struct{}

// noteGreenBuild tells the run's record, if ctx carries one, that a build
// inside it went green.
func noteGreenBuild(ctx context.Context, buildID string) {
	run, ok := ctx.Value(buildVersionsKey{}).(*buildVersions)
	if !ok {
		return
	}

	run.mu.Lock()
	defer run.mu.Unlock()

	run.lastGreen = buildID
}

// runBuildID is the build a run's own versions are recorded under.
func (b *buildVersions) runBuildID(runID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.lastGreen != "" {
		return b.lastGreen
	}

	return runID
}

// maxRecordedVersionBytes caps a put's version. Put stdout usually echoes a
// remote API response, and a version becomes a key in three tables and is
// rendered into downstream commands.
//
// ponytail: a check has no equivalent cap; upgrade is one shared store limit.
const maxRecordedVersionBytes = 4 << 10

func withBuildVersions(ctx context.Context) (context.Context, *buildVersions) {
	versions := &buildVersions{by: map[string]map[string]bool{}}

	return context.WithValue(ctx, buildVersionsKey{}, versions), versions
}

// versionRecordable encodes version for recording, or reports false: nothing
// printed records nothing, and an oversized version is printed and warned
// about rather than stored, which keeps the gate shut.
func versionRecordable(ctx context.Context, resource string, version map[string]any) (string, bool) {
	if len(version) == 0 {
		return "", false
	}

	encoded, err := store.EncodeVersion(version)
	if err != nil {
		slog.Warn("job.version_unrecordable", "resource", resource, "error", err)

		return "", false
	}

	if len(encoded) > maxRecordedVersionBytes {
		fmt.Printf("warning: version of %s is over %d bytes and is not recorded\n", resource, maxRecordedVersionBytes)
		logFrom(ctx).Warn("job.version_too_large", "resource", resource, "bytes", len(encoded))

		return "", false
	}

	return encoded, true
}

// recordBuildVersion notes a version this build fetched or put. Best-effort:
// this is bookkeeping for a downstream constraint and not the work the step
// was asked to do.
func recordBuildVersion(ctx context.Context, resource string, version map[string]any) {
	versions, ok := ctx.Value(buildVersionsKey{}).(*buildVersions)
	if !ok {
		return
	}

	encoded, ok := versionRecordable(ctx, resource, version)
	if !ok {
		return
	}

	versions.mu.Lock()
	defer versions.mu.Unlock()

	if versions.by[resource] == nil {
		versions.by[resource] = map[string]bool{}
	}

	versions.by[resource][encoded] = true
}

// recordPutOrder fixes a put's version in the resource's history when the put
// runs, so its order follows publish time rather than the random order the
// build-end recording would give. It is not a green claim.
func recordPutOrder(ctx context.Context, st store.Versions, resource string, version map[string]any) {
	encoded, ok := versionRecordable(ctx, resource, version)
	if !ok {
		return
	}

	_, err := st.RecordVersionOrder(context.WithoutCancel(ctx), resource, encoded)
	if err != nil {
		logFrom(ctx).Warn("job.put_order_unrecorded", "resource", resource, "error", err)
	}
}

// recordResolvedVersion mirrors a get step's fetched version into
// resource_checks — the table the web UI's resources page reads — for a
// resource nothing polls. A resource config.Config.ResourceIsPolled (the
// single-name form of PolledResourceNames, which internal/trigger's poller
// uses) answers yes for is skipped: that table doubles as the poller's
// dirty-bit baseline, and this run's version is not necessarily the poller's
// latest (a passed:-constrained get fetches whatever version satisfied the
// constraint, which can be older than the current head) — recording it
// there would either suppress a real trigger or show a stale-looking
// "latest" for a resource the poller already tracks correctly.
//
// pinned is the same skip fanOutGet's takeSet gives a --pin'd run (see
// planWalk.pinned): naming a version is an instruction outside the normal
// discovery flow, so an older, explicitly-pinned fetch must not overwrite an
// unpolled resource's displayed "last checked" version with something older
// than what an unpinned run already recorded — display would regress, and
// the next refresh's cursor (checkCursorFor, internal/pipeline/refresh.go)
// would re-walk ground it already covered.
//
// Best-effort, same posture as recordBuildVersion: this is bookkeeping for
// a page, not the work the step was asked to do.
func recordResolvedVersion(ctx context.Context, st store.Store, cfg *config.Config, resourceName string, version map[string]any, pinned bool) {
	if pinned || cfg.ResourceIsPolled(resourceName) {
		return
	}

	encoded, err := store.EncodeVersion(version)
	if err != nil {
		slog.Warn("job.resolved_version_unrecordable", "resource", resourceName, "error", err)

		return
	}

	err = st.RecordCheckedVersion(ctx, resourceName, encoded)
	if err != nil {
		slog.Warn("job.resolved_version_unrecorded", "resource", resourceName, "error", err)
	}
}

// recordPassedVersions marks every version a successful BUILD fetched or
// put as green for its job.
//
// Per build, not per step: passed: means "that job ran green against this
// exact version", and a build that failed after its get proves nothing about
// the version it fetched. buildID ties one build's versions together, so a
// downstream fan-in can ask whether they were green TOGETHER rather than
// merely each-at-some-point.
//
// A run fans out into one build per input set, and each records its own
// versions under its own id (see runTriggeredBuild). Recording once per JOB
// instead was wrong in both directions: it correlated versions from different
// sets that never ran together, and — the map being keyed per resource — it
// kept only the last set's, so every earlier set's versions stayed invisible
// to a downstream gate forever. They could not be recovered later either: an
// exhausted input holds at its NEWEST covered version, so a version
// superseded within one run is never bound again.
func recordPassedVersions(ctx context.Context, st store.Store, jobName, buildID string, fetched *buildVersions) {
	fetched.mu.Lock()
	defer fetched.mu.Unlock()

	recCtx := context.WithoutCancel(ctx)

	for resource, versions := range fetched.by {
		for version := range versions {
			err := st.RecordPassedVersion(recCtx, jobName, resource, version, buildID)
			if err != nil {
				slog.Warn("job.passed_unrecorded", "job", jobName, "resource", resource, "error", err)
			}
		}
	}
}

// VersionSetPassedUpstream reports whether upstreamJob has one build in which
// every (resource, version) pair in versions was green at once.
//
// It takes a SET rather than one resource at a time, which is the whole
// correction. Asking per resource admits a downstream job running against a
// combination of versions that each passed upstream in different builds and
// never passed together — a fan-in of "the repo" and "the config" that were
// individually fine and jointly untested. Concourse resolves passed: across a
// whole plan at once for this reason; see docs/conformance.md.
//
// Exported for internal/trigger, which is where the constraint bites: a set
// that has not passed upstream must not enqueue the downstream job at all,
// rather than starting it and discovering the problem later.
func VersionSetPassedUpstream(ctx context.Context, st store.Versions, upstreamJob string, versions map[string]map[string]any) (bool, error) {
	want := make(map[string]string, len(versions))

	for resource, version := range versions {
		encoded, err := json.Marshal(version)
		if err != nil {
			// An unrenderable version cannot be matched against anything, so
			// the safe answer is "not yet" — passed: exists to hold work back.
			return false, nil //nolint:nilerr // deliberately conservative; the constraint is a gate, not a hint
		}

		want[resource] = string(encoded)
	}

	passed, err := st.HasPassedVersionSet(ctx, upstreamJob, want)
	if err != nil {
		return false, err //nolint:wrapcheck // HasPassedVersionSet already names the job
	}

	return passed, nil
}

// PassedConstraintsFor is config.Job.PassedConstraints, re-exported so
// internal/trigger does not have to reach for the config type's method on a
// value it already has as a pointer.
func PassedConstraintsFor(job *config.Job) map[string][]string {
	return job.PassedConstraints()
}
