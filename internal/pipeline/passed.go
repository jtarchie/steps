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

// recordPassedVersions marks every version this job fetched as green for it,
// once the job as a whole has succeeded.
//
// Per JOB, not per step: passed: means "that job ran green against this exact
// version", and a job that failed after its get proves nothing about the
// version it fetched.
func recordPassedVersions(ctx context.Context, st *store.Store, jobName string, fetched *fetchedVersions) {
	fetched.mu.Lock()
	defer fetched.mu.Unlock()

	recCtx := context.WithoutCancel(ctx)

	for resource, version := range fetched.by {
		err := st.RecordPassedVersion(recCtx, jobName, resource, version)
		if err != nil {
			slog.Warn("job.passed_unrecorded", "job", jobName, "resource", resource, "error", err)
		}
	}
}

// VersionPassedUpstream reports whether every job a constraint names has
// already succeeded against this exact version.
//
// Exported for internal/trigger, which is where the constraint actually bites:
// a version that has not passed upstream must not enqueue the downstream job
// at all, rather than starting it and discovering the problem later.
func VersionPassedUpstream(ctx context.Context, st *store.Store, upstream []string, resource string, version map[string]any) (bool, error) {
	encoded, err := json.Marshal(version)
	if err != nil {
		// An unrenderable version cannot be matched against anything, so the
		// safe answer is "not yet" — passed: exists to hold work back.
		return false, nil //nolint:nilerr // deliberately conservative; the constraint is a gate, not a hint
	}

	for _, jobName := range upstream {
		passed, lookupErr := st.HasPassedVersion(ctx, jobName, resource, string(encoded))
		if lookupErr != nil {
			return false, lookupErr //nolint:wrapcheck // HasPassedVersion already names the job
		}

		if !passed {
			return false, nil
		}
	}

	return true, nil
}

// PassedConstraintsFor is config.Job.PassedConstraints, re-exported so
// internal/trigger does not have to reach for the config type's method on a
// value it already has as a pointer.
func PassedConstraintsFor(job *config.Job) map[string][]string {
	return job.PassedConstraints()
}
