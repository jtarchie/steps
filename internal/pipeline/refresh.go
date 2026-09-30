package pipeline

// Refresh on run: whatever triggers a job, its resources are re-checked
// before anything resolves.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// refreshResourceHistory runs every get resource's check once and files what
// it reports, so the run that follows resolves against the world as of NOW
// rather than as of whenever something last happened to look.
//
// The gap this closes is widest for a manual `steps run`: a polled resource's
// history is only as fresh as the last watch poll, which may be days old —
// while, perversely, an UNTRIGGERED get got a live check via the resolution
// fallback. It also narrows the queue-wait gap: a job that sat behind a slow
// worker binds versions as of claim time, not enqueue time. This is the
// poll-based approximation of Concourse's model, where a build is created
// against a version DB that checks feed continuously.
//
// Four deliberate boundaries:
//
//   - The check CURSOR is read, never advanced. resource_checks doubles as
//     the watcher's dirty baseline, and a run moving it would suppress
//     triggers for versions no poll ever dispatched — the exact defect that
//     split reading from writing in the first place.
//   - A failed check WARNS and the run proceeds on recorded history. The
//     version record is the truth and checks feed it; a check outage must
//     not block building what is already known. A resource with no history
//     at all still fails downstream, at resolution, where "no versions
//     available" already names it.
//   - Explain does not refresh: `steps plan` stays read-only and describes
//     the run that current knowledge implies.
//   - Under a daemon, a POLLED resource on a worker acquired on demand is not
//     refreshed: the check would start or launch a machine for an answer the
//     poller is already keeping fresh. Only when both hold — a resource
//     nothing polls, or a one-shot command with no poller, would otherwise
//     never see a new version at all, since a run's own resolution records
//     history the next run then reuses.
//
// Known narrowing, stated here because this is what causes it: history a run
// records mutes the next poll's new-version signal for those versions. The
// head-vs-baseline comparison still fires on any head change, so the only
// case that fails to self-trigger is a run-discovered backfill BELOW an
// unchanged head — which then builds on the next genuine trigger instead of
// its own.
func refreshResourceHistory(ctx context.Context, cfg *config.Config, st store.Store, job *config.Job) {
	for _, name := range job.GetResourceNames() {
		refreshOneResource(ctx, cfg, st, job, name)
	}
}

func refreshOneResource(ctx context.Context, cfg *config.Config, st store.Store, job *config.Job, name string) {
	resource, err := cfg.FindResource(name)
	if err != nil {
		return // an unresolvable resource is a load error long before here
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return
	}

	if tag, skip := refreshAcquires(ctx, cfg, name); skip {
		notef(ctx, "not refreshing %s before the plan: worker %s is acquired on demand and the poller keeps its history; building from what it last recorded", name, tag)

		return
	}

	cursor, err := checkCursorFor(ctx, st, name)
	if err != nil {
		warnRefreshFailed(ctx, name, err)

		return
	}

	// Recorded under the check's own label, having no node; installed BEFORE PlaceResource, because the placer notes its runner on the context it was resolved under.
	ctx, placed := withPlacementSink(ctx)
	defer recordPlacement(ctx, stepRunner{cfg: cfg, jobName: job.Name, st: st}, placed, 0, "check "+name, "check "+name, "")

	placedCtx, err := PlaceResource(ctx, cfg, name)
	if err != nil {
		warnRefreshFailed(ctx, name, err)

		return
	}

	ctx = placedCtx

	versions, err := rsrc.CheckVersions(ctx, cfg, *resourceType, resource.Env, resource.Source, cursor)
	if err != nil {
		warnRefreshFailed(ctx, name, err)

		return
	}

	if len(versions) == 0 {
		return
	}

	_, err = st.RecordVersions(ctx, name, versions, cfg.VersionHistoryLimit())
	if err != nil {
		warnRefreshFailed(ctx, name, err)
	}
}

// refreshAcquires names the tag whose machine a refresh of this resource would have to acquire, when the poller makes that acquisition redundant.
func refreshAcquires(ctx context.Context, cfg *config.Config, name string) (string, bool) {
	tag := placementTag(resourceStep(cfg, name))
	if tag == "" || !SharesWorkers(ctx) || !acquiredOnDemand(ctx, tag) {
		return "", false
	}

	return tag, cfg.ResourceIsPolled(name)
}

// checkCursorFor reads the recorded check cursor, decoded — the same value a
// watch poll hands the check, so a cursor-driven type answers the refresh as
// cheaply as it answers a poll.
func checkCursorFor(ctx context.Context, st store.Store, name string) (map[string]any, error) {
	last, found, err := st.LastChecked(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("reading the check cursor for %q: %w", name, err)
	}

	if !found {
		// No cursor recorded is the normal first-contact state, not an error.
		return nil, nil //nolint:nilnil // absence is the meaning
	}

	cursor, err := rsrc.ParseVersionJSON(last.Version)
	if err != nil {
		return nil, fmt.Errorf("reading the check cursor for %q: %w", name, err)
	}

	return cursor, nil
}

func warnRefreshFailed(ctx context.Context, name string, err error) {
	warnf(ctx, "could not refresh %s; building from recorded history: %v", name, err)
	slog.WarnContext(ctx, "job.refresh_failed", "resource", name, "error", err)
}
