package pipeline

// The `get: version: every` cursor: which versions a job has already fanned
// out over, so a second trigger does not redo the first one's work.
//
// Why this is not the merkle cache's job, since that is the first place anyone
// looks: the version IS part of a get node's hashed content, so a chain re-run
// against a version it already ran hashes identically and is skipped. But
// route.go's unskippableReason deliberately never skips an `agent:` or a
// `put:` — one is non-deterministic, the other's whole worth is an effect on
// the outside world — so a plan containing either re-executes for every
// version the check still returns. The cache avoids recomputing a VALUE; it
// was never a mechanism for avoiding repeating an EFFECT. That belongs one
// layer up, in deciding which versions to fan out over at all.
//
// Concourse does the same thing with a per-job cursor over versions the job
// has not built: NextEveryVersion (atc/db/versions_db.go) reads the versions a
// build was CREATED with (build_resource_config_version_inputs) and filters on
// build status nowhere, so a version taken by a failed build stays taken. This
// follows that exactly — see runGetStep. What this cannot do is resurrect:
// it decides which of the versions steps HAS that a job still owes work on.
// Which versions steps has is resource_versions' answer, and a version
// pruned from there is beyond both.

import (
	"context"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// versionCursor answers "has this job already taken this version", for the
// resources one job's plan fans out over.
type versionCursor struct {
	// marks is resource name -> the highest check_order this job has taken.
	// Loaded once per run, before planning, so the plan-time and run-time
	// views cannot drift apart mid-run as versions are consumed.
	marks map[string]int64
	// orders maps a version's canonical JSON to its check_order, for the
	// resources this job reads. A version with no entry has never been
	// recorded, so it sits above every mark and is still to do.
	orders map[string]map[string]int64

	// suppress is false under --force, which re-runs everything the cursor
	// would otherwise filter out. It gates only `has`: the run still RECORDS
	// what it took, because a forced run performs the effects just like any
	// other, and a version it completed must not be taken a third time by the
	// next ordinary run. Forcing is "ignore what was taken", not "forget what
	// this run is doing".
	suppress bool
}

// loadVersionCursor reads the consumed set for every resource this job fans
// out over. A job with no `version: every` get reads nothing and allocates
// nothing, which is nearly every job.
//
// A store failure is returned rather than swallowed: guessing "nothing is
// consumed" would re-run every visible version — the exact behaviour this
// exists to stop — and guessing the opposite would silently skip real work.
//
// suppress is false under --force. The cursor is still built and still
// records, so the versions a forced run completes are marked taken; only the
// filtering is switched off.
func loadVersionCursor(ctx context.Context, st *store.Store, job *config.Job, suppress bool) (*versionCursor, error) {
	var resources []string

	seen := map[string]bool{}

	for _, step := range job.Plan {
		if step.Get == "" || !fansOutOverEveryVersion(step) {
			continue
		}

		name := step.GetResourceName()
		if seen[name] {
			continue
		}

		seen[name] = true

		resources = append(resources, name)
	}

	if len(resources) == 0 {
		return nil, nil //nolint:nilnil // "no cursor needed" is the common case, and a nil *versionCursor is a valid receiver
	}

	cursor := &versionCursor{
		marks:    make(map[string]int64, len(resources)),
		orders:   make(map[string]map[string]int64, len(resources)),
		suppress: suppress,
	}

	for _, name := range resources {
		mark, err := st.ConsumedMark(ctx, job.Name, name)
		if err != nil {
			return nil, err //nolint:wrapcheck // ConsumedMark already names the job
		}

		orders, err := st.VersionOrders(ctx, name)
		if err != nil {
			return nil, err //nolint:wrapcheck // VersionOrders already names the resource
		}

		cursor.marks[name] = mark
		cursor.orders[name] = orders
	}

	return cursor, nil
}

// The run and plan paths do not read the poller's check cursor
// (resource_checks), and must not: it means "the newest version the POLLER
// has dispatched" and it advances as soon as a poll enqueues work, so a job
// re-deriving its versions against it asks a different question than the
// poll did and gets nothing. That is not a limitation any more, because they
// no longer re-derive at all — they read history.
//
// resourceHistory answers "which versions of this resource exist", from
// resource_versions, in the order they were discovered.
//
// It replaces handing versions to a job on its queue row. The poll's answer
// used to have to be CARRIED because asking a cursor-driven check twice
// returns two different answers; a lookup is repeatable, which is what makes
// plan time and run time able to agree without anything being threaded
// between them.
//
// What stops a job repeating work is unchanged and separate: the consumed
// set above, keyed to what a job actually took.
type resourceHistory struct {
	versions map[string][]map[string]any
	// gated marks resources whose list is a passed: verdict rather than raw
	// history. For those, EMPTY is an answer — "nothing has passed" — and
	// must not fall back to a check that would resolve around the gate.
	gated map[string]bool
}

// loadResourceHistory reads the history of every resource this job gets, once
// up front — the same reason the consumed set is read once: the planner and
// the executor have to judge the same list, and a lazy per-resource read
// could see a poll land between them.
func loadResourceHistory(ctx context.Context, st *store.Store, job *config.Job) (*resourceHistory, error) {
	history := &resourceHistory{versions: map[string][]map[string]any{}, gated: map[string]bool{}}

	// A passed:-constrained resource resolves among GREEN versions, not raw
	// history. Enforcing the gate here — at resolution — is what makes it a
	// gate at all: checked only at trigger time, it judged a world that could
	// change before a worker claimed the job (a newer, untested version
	// arriving in between was then built as "latest"), and a manual
	// `steps run` never consulted it in the first place. Resolution is the
	// one door every run comes through.
	//
	// The list may legitimately be EMPTY — nothing has passed yet — and empty
	// is an answer, not an absence: the get fails with "no versions
	// available" rather than falling back to a check that would bypass the
	// gate. Note a --pin still overrides; naming a version is an instruction.
	constraints := job.PassedConstraints()

	for _, name := range job.GetResourceNames() {
		var (
			versions []map[string]any
			err      error
		)

		if upstreams, constrained := constraints[name]; constrained {
			history.gated[name] = true

			versions, err = st.GreenVersions(ctx, name, upstreams)
			if versions == nil {
				versions = []map[string]any{}
			}
		} else {
			versions, err = st.ResourceVersions(ctx, name)
		}

		if err != nil {
			return nil, err //nolint:wrapcheck // the store names the resource
		}

		history.versions[name] = versions
	}

	return history, nil
}

// get returns the versions recorded for a resource, or nil when there are
// none — which resource.ResolveVersions treats as "nothing supplied, run the
// check". That is the right fallback: a resource nothing has ever polled has
// no history to read, and a manual `steps run` against one must still work.
func (h *resourceHistory) get(resourceName string) []map[string]any {
	if h == nil {
		return nil
	}

	versions := h.versions[resourceName]

	// A gated resource's list is authoritative even when empty: "nothing has
	// passed" must fail the get, not run a check the gate never sees.
	if len(versions) == 0 && !h.gated[resourceName] {
		return nil
	}

	return versions
}

// fansOutOverEveryVersion reports whether a get step uses version: every —
// the only mode that runs the rest of the plan once per version.
func fansOutOverEveryVersion(step config.Step) bool {
	mode, _ := rsrc.VersionMode(step)

	return mode == "every"
}

// has reports whether this job has already taken the version. A nil cursor
// (no version: every in the plan) has taken nothing, and neither has one
// under --force, which re-runs everything by design.
func (c *versionCursor) has(resourceName string, version map[string]any) bool {
	if c == nil || !c.suppress {
		return false
	}

	key, ok := encodeVersion(version)
	if !ok {
		// An unencodable version cannot be placed in the order, so it cannot
		// be suppressed either. Running it again is the recoverable failure;
		// skipping work that was never recorded is not.
		return false
	}

	// A version with no recorded order has never been seen, so it is above
	// the mark by definition — the same conclusion, reached without inventing
	// an order for it.
	order, seen := c.orders[resourceName][key]
	if !seen {
		return false
	}

	return order <= c.marks[resourceName]
}

// take records that this job has taken a version — called as its build
// STARTS, whatever that build then does, which is Concourse's rule (see the
// call site in runGetStep for the source it comes from).
//
// It records on a context detached from the build's, so a version whose build
// was already under way is not handed out again purely because the process was
// on its way out.
//
// Best-effort by design: failing to record must not turn a running build into
// a failed one. The cost of a lost row is that the version is taken once more
// later, which is the direction this errs on everywhere.
func (c *versionCursor) take(
	ctx context.Context, st *store.Store, jobName, resourceName string, version map[string]any,
) {
	if c == nil {
		return
	}

	key, ok := encodeVersion(version)
	if !ok {
		return
	}

	detached := context.WithoutCancel(ctx)

	// The version's order, filing it first if this job resolved it itself —
	// which is every `steps run` against a resource no poll has recorded.
	// Without that the cursor could not advance past it and the fan-out would
	// repeat on the next run.
	order, err := st.RecordVersionOrder(detached, resourceName, key)
	if err != nil {
		slog.Warn("job.cursor_unrecorded", "job", jobName, "resource", resourceName, "error", err)

		return
	}

	err = st.RecordConsumedMark(detached, jobName, resourceName, order)
	if err != nil {
		slog.Warn("job.cursor_unrecorded", "job", jobName, "resource", resourceName, "error", err)

		return
	}

	if c.orders[resourceName] == nil {
		c.orders[resourceName] = map[string]int64{}
	}

	c.orders[resourceName][key] = order

	if order > c.marks[resourceName] {
		c.marks[resourceName] = order
	}
}

// encodeVersion renders a version the canonical way every table keys on. A
// thin adapter over store.EncodeVersion so the "same version, same string"
// rule has one implementation.
func encodeVersion(version map[string]any) (string, bool) {
	encoded, err := store.EncodeVersion(version)
	if err != nil {
		return "", false
	}

	return encoded, true
}
