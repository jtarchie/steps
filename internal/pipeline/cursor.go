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
// follows that exactly — see runGetStep, and docs/conformance.md for the one
// divergence that remains (steps keeps no version history, so this can only
// suppress a version, never resurrect one that scrolled out of view).

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
)

// versionCursor answers "has this job already taken this version", for the
// resources one job's plan fans out over.
type versionCursor struct {
	// consumed is resource name -> set of version JSONs. Loaded once per run,
	// before planning, so the plan-time and run-time views cannot drift apart
	// mid-run as versions are consumed.
	consumed map[string]map[string]bool

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
		consumed: make(map[string]map[string]bool, len(resources)),
		suppress: suppress,
	}

	for _, name := range resources {
		consumed, err := st.ConsumedVersions(ctx, job.Name, name)
		if err != nil {
			return nil, err //nolint:wrapcheck // ConsumedVersions already names the job
		}

		cursor.consumed[name] = consumed
	}

	return cursor, nil
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
		// An unencodable version cannot be matched against the table, so it
		// cannot be suppressed either. Running it again is the recoverable
		// failure; skipping work that was never recorded is not.
		return false
	}

	return c.consumed[resourceName][key]
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
func (c *versionCursor) take(ctx context.Context, st *store.Store, jobName, resourceName string, version map[string]any) {
	if c == nil {
		return
	}

	key, ok := encodeVersion(version)
	if !ok {
		return
	}

	err := st.RecordConsumedVersion(context.WithoutCancel(ctx), jobName, resourceName, key)
	if err != nil {
		slog.Warn("job.cursor_unrecorded", "job", jobName, "resource", resourceName, "error", err)

		return
	}

	if c.consumed[resourceName] == nil {
		c.consumed[resourceName] = map[string]bool{}
	}

	c.consumed[resourceName][key] = true
}

// encodeVersion renders a version the same way passed: does (json.Marshal,
// whose map keys are sorted), so both tables key on identical strings.
func encodeVersion(version map[string]any) (string, bool) {
	encoded, err := json.Marshal(version)
	if err != nil {
		return "", false
	}

	return string(encoded), true
}
