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

// RecordCheckedVersion upserts the latest observed version JSON for
// resourceName, independent of whether any job triggered by the change
// succeeds — version-checking and build outcomes are tracked separately,
// mirroring how job_runs only ever records succeeded chains.
func (s *Store) RecordCheckedVersion(ctx context.Context, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resource_checks (pipeline_id, resource_name, version_json, checked_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(pipeline_id, resource_name) DO UPDATE SET
			version_json = excluded.version_json,
			checked_at   = excluded.checked_at
	`, s.pipelineID, resourceName, versionJSON, now())
	if err != nil {
		return fmt.Errorf("could not record checked version for %q: %w", resourceName, err)
	}

	return nil
}

// LastChecked returns one resource's last-checked row, with found=false when
// nothing has checked it yet — the single-resource counterpart to
// CheckedResources, for a caller that wants one row rather than every
// resource's just to look one up.
//
// The version JSON on it is the check cursor: what the next check is handed to
// say "anything after this", and still the resource's current version when a
// check reports nothing new.
func (s *Store) LastChecked(ctx context.Context, resourceName string) (CheckedResource, bool, error) {
	var (
		row       CheckedResource
		checkedAt string
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT resource_name, version_json, checked_at FROM resource_checks WHERE pipeline_id = ? AND resource_name = ?`,
		s.pipelineID, resourceName,
	).Scan(&row.Name, &row.Version, &checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckedResource{}, false, nil
	}

	if err != nil {
		return CheckedResource{}, false, fmt.Errorf("could not query resource_checks: %w", err)
	}

	row.CheckedAt = parseTimestamp(checkedAt)

	return row, true, nil
}

// CheckedResources lists every resource version the watcher has recorded.
func (s *Store) CheckedResources(ctx context.Context) ([]CheckedResource, error) {
	return collect(ctx, s.db, "resource checks",
		`SELECT resource_name, version_json, checked_at FROM resource_checks
		 WHERE pipeline_id = ? ORDER BY resource_name`,
		[]any{s.pipelineID}, func(rows *sql.Rows) (CheckedResource, error) {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not record a passed version for job %q: %w", jobName, err)
	}

	defer func() { _ = tx.Rollback() }()

	// The version has to exist in history before anything may reference it.
	// A `steps run` resolves its own versions and never goes near the poller,
	// so this is the path by which a manually-run job's versions enter
	// history at all.
	err = ensureVersion(ctx, tx, s.pipelineID, resourceName, versionJSON)
	if err != nil {
		return err
	}

	// On conflict the build_id is UPDATED rather than left alone: re-running a
	// job against versions it has already passed should refresh which build
	// vouches for them, or the row would keep pointing at the oldest build
	// that saw it and a newer coherent set would be invisible.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO job_versions (pipeline_id, job_name, resource_name, version_json, recorded_at, build_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (pipeline_id, job_name, resource_name, version_json)
		DO UPDATE SET build_id = excluded.build_id, recorded_at = excluded.recorded_at
	`, s.pipelineID, jobName, resourceName, versionJSON, now(), buildID)
	if err != nil {
		return fmt.Errorf("could not record a passed version for job %q: %w", jobName, err)
	}

	err = tx.Commit()
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
		FROM job_versions WHERE pipeline_id = ? AND job_name = ?
		ORDER BY recorded_at DESC LIMIT ?
	`, []any{s.pipelineID, jobName, limit}, func(rows *sql.Rows) (PassedVersion, error) {
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
	args := []any{s.pipelineID, jobName}

	for resourceName, versionJSON := range want {
		clauses = append(clauses, "(resource_name = ? AND version_json = ?)")
		args = append(args, resourceName, versionJSON)
	}

	args = append(args, len(want))

	query := `
		SELECT 1 FROM job_versions
		WHERE pipeline_id = ? AND job_name = ? AND build_id <> ''
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

// ConsumedMark is how far a job has fanned out over a resource: the highest
// check_order it has taken. Zero means it has taken nothing, and since
// check_order starts at 1 that reads as "everything is still to do".
func (s *Store) ConsumedMark(ctx context.Context, jobName, resourceName string) (int64, error) {
	var mark sql.NullInt64

	err := s.db.QueryRowContext(ctx,
		`SELECT check_order FROM job_version_cursor
		 WHERE pipeline_id = ? AND job_name = ? AND resource_name = ?`,
		s.pipelineID, jobName, resourceName).Scan(&mark)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("could not read the cursor for job %q: %w", jobName, err)
	}

	return mark.Int64, nil
}

// RecordConsumedMark advances a job's cursor to include order.
//
// Only ever forward. A run that takes an older version than one already taken
// -- a backfill reaching a job that has moved past it -- must not rewind the
// mark and offer everything in between a second time.
//
// No pruning and no cap, which is the point of a mark: there are no members
// to forget, so it cannot develop the holes a capped set does. See the table
// comment in schema.go.
func (s *Store) RecordConsumedMark(ctx context.Context, jobName, resourceName string, order int64) error {
	if order <= 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_version_cursor (pipeline_id, job_name, resource_name, check_order)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (pipeline_id, job_name, resource_name)
		DO UPDATE SET check_order = MAX(check_order, excluded.check_order)
	`, s.pipelineID, jobName, resourceName, order)
	if err != nil {
		return fmt.Errorf("could not record the cursor for job %q: %w", jobName, err)
	}

	return nil
}

// RecordRunInput remembers that a run's build was created with this version of
// this resource — the record --resume needs to reach a version the cursor has
// already taken. See the run_inputs DDL for why the cursor cannot answer it.
//
// The run is the scope rather than the build: --resume continues a run, and a
// version any of its builds took is one it may have to reach again.
func (s *Store) RecordRunInput(ctx context.Context, runID, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_inputs (run_id, resource_name, version_json)
		VALUES (?, ?, ?)
		ON CONFLICT (run_id, resource_name, version_json) DO NOTHING
	`, runID, resourceName, versionJSON)
	if err != nil {
		return fmt.Errorf("could not record the inputs of run %q: %w", runID, err)
	}

	return nil
}

// RunInputs reports the versions a run's builds were created with, as
// resource name -> the canonical JSON of each version.
//
// No pipeline_id predicate: run_inputs reaches its pipeline through runs(id),
// which is how every other run-scoped table is scoped, and runID is already
// unique across the file.
func (s *Store) RunInputs(ctx context.Context, runID string) (map[string]map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT resource_name, version_json FROM run_inputs WHERE run_id = ?
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("could not read the inputs of run %q: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	inputs := map[string]map[string]bool{}

	for rows.Next() {
		var resourceName, versionJSON string

		err = rows.Scan(&resourceName, &versionJSON)
		if err != nil {
			return nil, fmt.Errorf("could not read the inputs of run %q: %w", runID, err)
		}

		if inputs[resourceName] == nil {
			inputs[resourceName] = map[string]bool{}
		}

		inputs[resourceName][versionJSON] = true
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read the inputs of run %q: %w", runID, err)
	}

	return inputs, nil
}
