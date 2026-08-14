package store

// Resource versions, in the three different questions the pipeline asks about
// them: what a check last saw, what a job went green on (passed:), and what a
// job has already fanned out over (get: version: every).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CheckedResource is a resource's last observed version.
type CheckedResource struct {
	Name      string
	Version   string
	CheckedAt time.Time
}

// PassedVersion is one resource version a job succeeded against.
type PassedVersion struct {
	Resource   string
	Version    string
	RecordedAt time.Time
}

// LastCheckedVersion returns the JSON of the most recently recorded version
// for resourceName, or found=false if it's never been checked.
func (s *Store) LastCheckedVersion(ctx context.Context, resourceName string) (string, bool, error) {
	var versionJSON string

	err := s.db.QueryRowContext(ctx,
		`SELECT version_json FROM resource_checks WHERE resource_name = ?`,
		resourceName,
	).Scan(&versionJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("could not query resource_checks: %w", err)
	}

	return versionJSON, true, nil
}

// RecordCheckedVersion upserts the latest observed version JSON for
// resourceName, independent of whether any job triggered by the change
// succeeds — version-checking and build outcomes are tracked separately,
// mirroring how job_runs only ever records succeeded chains.
func (s *Store) RecordCheckedVersion(ctx context.Context, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resource_checks (resource_name, version_json, checked_at)
		VALUES (?, ?, ?)
		ON CONFLICT(resource_name) DO UPDATE SET
			version_json = excluded.version_json,
			checked_at   = excluded.checked_at
	`, resourceName, versionJSON, now())
	if err != nil {
		return fmt.Errorf("could not record checked version for %q: %w", resourceName, err)
	}

	return nil
}

// CheckedResources lists every resource version the watcher has recorded.
func (s *Store) CheckedResources(ctx context.Context) ([]CheckedResource, error) {
	return collect(ctx, s.db, "resource checks",
		`SELECT resource_name, version_json, checked_at FROM resource_checks ORDER BY resource_name`,
		nil, func(rows *sql.Rows) (CheckedResource, error) {
			var (
				row       CheckedResource
				checkedAt string
			)

			err := rows.Scan(&row.Name, &row.Version, &checkedAt)
			row.CheckedAt = parseTimestamp(checkedAt)

			return row, err //nolint:wrapcheck // collect wraps with the thing being read
		})
}

// RecordPassedVersion records that jobName completed successfully against this
// exact version of a resource. It is what a downstream job's passed: reads.
func (s *Store) RecordPassedVersion(ctx context.Context, jobName, resourceName, versionJSON, buildID string) error {
	// On conflict the build_id is UPDATED rather than left alone: re-running a
	// job against versions it has already passed should refresh which build
	// vouches for them, or the row would keep pointing at the oldest build
	// that saw it and a newer coherent set would be invisible.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_versions (job_name, resource_name, version_json, recorded_at, build_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (job_name, resource_name, version_json)
		DO UPDATE SET build_id = excluded.build_id, recorded_at = excluded.recorded_at
	`, jobName, resourceName, versionJSON, now(), buildID)
	if err != nil {
		return fmt.Errorf("could not record a passed version for job %q: %w", jobName, err)
	}

	return nil
}

// PassedVersions lists the resource versions a job has recorded as green —
// what a downstream passed: constraint is satisfied by.
func (s *Store) PassedVersions(ctx context.Context, jobName string, limit int) ([]PassedVersion, error) {
	return collect(ctx, s.db, "job versions", `
		SELECT resource_name, version_json, recorded_at
		FROM job_versions WHERE job_name = ?
		ORDER BY recorded_at DESC LIMIT ?
	`, []any{jobName, limit}, func(rows *sql.Rows) (PassedVersion, error) {
		var (
			row        PassedVersion
			recordedAt string
		)

		err := rows.Scan(&row.Resource, &row.Version, &recordedAt)
		row.RecordedAt = parseTimestamp(recordedAt)

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// HasPassedVersionSet reports whether jobName has one build in which EVERY
// (resource, version) pair in want was green at the same time.
//
// This is the correlated question, and the difference from asking per-resource
// is the whole point. Two versions that each passed upstream in different
// builds have never been proven to work TOGETHER, and a downstream fan-in that
// accepts them is running a combination nothing validated. Concourse resolves
// passed: across a whole plan at once for this reason; see docs/conformance.md.
// The per-resource form is deliberately not kept alongside — an available
// answer to the wrong question is how the bug comes back.
//
// Rows written before build_id existed carry ” and are excluded, so they can
// never satisfy a correlated match. Conservative on purpose: passed: is a gate.
//
// An empty want is vacuously true — there is no constraint to satisfy.
func (s *Store) HasPassedVersionSet(ctx context.Context, jobName string, want map[string]string) (bool, error) {
	if len(want) == 0 {
		return true, nil
	}

	// One OR-group per wanted pair, then a GROUP BY that demands a single
	// build_id matched all of them. COUNT(DISTINCT resource_name) rather than
	// COUNT(*) so a resource appearing twice under one build cannot stand in
	// for a resource that is missing.
	clauses := make([]string, 0, len(want))
	args := []any{jobName}

	for resourceName, versionJSON := range want {
		clauses = append(clauses, "(resource_name = ? AND version_json = ?)")
		args = append(args, resourceName, versionJSON)
	}

	args = append(args, len(want))

	query := `
		SELECT 1 FROM job_versions
		WHERE job_name = ? AND build_id <> ''
		  AND (` + strings.Join(clauses, " OR ") + `)
		GROUP BY build_id
		HAVING COUNT(DISTINCT resource_name) = ?
		LIMIT 1`

	var found int

	err := s.db.QueryRowContext(ctx, query, args...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("could not check passed versions for job %q: %w", jobName, err)
	}

	return true, nil
}

// consumedVersionCap bounds job_version_cursor per (job, resource). The rows
// exist to suppress versions a check can still return, so the bound has to be
// a count rather than an age: a version that is old but still visible must
// stay suppressed for as long as the check keeps offering it. A thousand is
// far past any check window anyone polls (Slack's is 20 messages, GitHub's a
// page) while keeping the table from growing forever.
const consumedVersionCap = 1000

// ConsumedVersions returns the set of version JSONs jobName has already fanned
// out over for resourceName under `get: version: every` — the cursor that
// stops the same version being taken twice.
func (s *Store) ConsumedVersions(ctx context.Context, jobName, resourceName string) (map[string]bool, error) {
	taken, err := collect(ctx, s.db, "consumed versions",
		`SELECT version_json FROM job_version_cursor WHERE job_name = ? AND resource_name = ?`,
		[]any{jobName, resourceName}, func(rows *sql.Rows) (string, error) {
			var versionJSON string

			return versionJSON, rows.Scan(&versionJSON)
		})
	if err != nil {
		return nil, err
	}

	consumed := make(map[string]bool, len(taken))
	for _, versionJSON := range taken {
		consumed[versionJSON] = true
	}

	return consumed, nil
}

// RecordConsumedVersion marks one version as taken by jobName. The caller
// (internal/pipeline's versionCursor) records only versions whose build
// succeeded, so a failure is retried — a documented divergence from
// Concourse's cursor, which advances regardless of build status (see
// docs/conformance.md). Re-recording is a no-op rather than an error, so a
// resumed or replayed run cannot fail on a version it already took.
func (s *Store) RecordConsumedVersion(ctx context.Context, jobName, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_version_cursor (job_name, resource_name, version_json, consumed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (job_name, resource_name, version_json) DO NOTHING
	`, jobName, resourceName, versionJSON, nowNano())
	if err != nil {
		return fmt.Errorf("could not record a consumed version for job %q: %w", jobName, err)
	}

	// Pruned here rather than on a timer: this is the only writer, so it is
	// the only place the bound can be enforced without a background loop.
	//
	// Ordered by rowid alone, NOT by consumed_at. consumed_at is RFC3339Nano
	// text, whose fractional part Go writes with trailing zeros trimmed — so
	// ".1Z" (100ms) sorts AFTER ".15Z" (150ms) lexically, and the newest rows
	// are not reliably the ones kept at the eviction boundary. rowid is
	// monotonic in insertion order, which is the order this actually means.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM job_version_cursor
		WHERE job_name = ? AND resource_name = ? AND rowid NOT IN (
			SELECT rowid FROM job_version_cursor
			WHERE job_name = ? AND resource_name = ?
			ORDER BY rowid DESC
			LIMIT ?
		)
	`, jobName, resourceName, jobName, resourceName, consumedVersionCap)
	if err != nil {
		return fmt.Errorf("could not prune consumed versions for job %q: %w", jobName, err)
	}

	return nil
}
