package pipeline

// The passed: constraint's run-time half: remembering which versions a job has
// actually been green against.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// fetchedVersions is what a job run actually fetched, per resource. A job that
// succeeds records these as "passed", which is the fact a downstream job's
// passed: constraint reads.
type fetchedVersions struct {
	mu sync.Mutex
	by map[string]string
}

type fetchedVersionsKey struct{}

func withFetchedVersions(ctx context.Context) (context.Context, *fetchedVersions) {
	fetched := &fetchedVersions{by: map[string]string{}}

	return context.WithValue(ctx, fetchedVersionsKey{}, fetched), fetched
}

// recordFetchedVersion notes the version a get step resolved to. Best-effort:
// a version that cannot be rendered as JSON is skipped rather than failing the
// step, since this is bookkeeping for a downstream constraint and not the work
// the step was asked to do.
func recordFetchedVersion(ctx context.Context, resource string, version map[string]any) {
	fetched, ok := ctx.Value(fetchedVersionsKey{}).(*fetchedVersions)
	if !ok {
		return
	}

	encoded, err := json.Marshal(version)
	if err != nil {
		slog.Warn("job.version_unrecordable", "resource", resource, "error", err)

		return
	}

	fetched.mu.Lock()
	defer fetched.mu.Unlock()

	fetched.by[resource] = string(encoded)
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
// Best-effort, same posture as recordFetchedVersion: this is bookkeeping for
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

// recordPassedVersions marks every version a successful BUILD fetched as
// green for its job.
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
func recordPassedVersions(ctx context.Context, st store.Store, jobName, buildID string, fetched *fetchedVersions) {
	fetched.mu.Lock()
	defer fetched.mu.Unlock()

	recCtx := context.WithoutCancel(ctx)

	for resource, version := range fetched.by {
		err := st.RecordPassedVersion(recCtx, jobName, resource, version, buildID)
		if err != nil {
			slog.Warn("job.passed_unrecorded", "job", jobName, "resource", resource, "error", err)
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
func VersionSetPassedUpstream(ctx context.Context, st store.Store, upstreamJob string, versions map[string]map[string]any) (bool, error) {
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
