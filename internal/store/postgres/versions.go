package postgres

// Resource versions, in the three questions the pipeline asks about them:
// what a check last saw, what a job went green on (passed:), and what a job
// has already fanned out over (get: version: every).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// RecordCheckedVersion upserts the latest observed version for a resource.
func (s *Store) RecordCheckedVersion(ctx context.Context, resourceName, versionJSON string) error {
	return recordCheckedVersion(ctx, s.db, s.pipelineID, resourceName, versionJSON)
}

func recordCheckedVersion(ctx context.Context, db executor, pipelineID int64, resourceName, versionJSON string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO resource_checks (pipeline_id, resource_name, version_json, checked_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (pipeline_id, resource_name) DO UPDATE SET
			version_json = excluded.version_json,
			checked_at   = excluded.checked_at
	`, pipelineID, resourceName, versionJSON, now())
	if err != nil {
		return fmt.Errorf("could not record checked version for %q: %w", resourceName, err)
	}

	return nil
}

// CompareAndSetCheckedVersion is one statement either way. Under READ
// COMMITTED the guarded UPDATE re-reads a row a concurrent writer changed
// before deciding, so the comparison is against what is committed, not what
// this statement's snapshot saw.
func (s *Store) CompareAndSetCheckedVersion(ctx context.Context, resourceName, expected string, expectedFound bool, next string) (bool, error) {
	query := `UPDATE resource_checks SET version_json = $1, checked_at = $2
		WHERE pipeline_id = $3 AND resource_name = $4 AND version_json = $5`
	args := []any{next, now(), s.pipelineID, resourceName, expected}

	if !expectedFound {
		query = `INSERT INTO resource_checks (pipeline_id, resource_name, version_json, checked_at)
			VALUES ($1, $2, $3, $4) ON CONFLICT (pipeline_id, resource_name) DO NOTHING`
		args = []any{s.pipelineID, resourceName, next, now()}
	}

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("could not advance checked version for %q: %w", resourceName, err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not advance checked version for %q: %w", resourceName, err)
	}

	return changed > 0, nil
}

// LastChecked returns one resource's last-checked row.
func (s *Store) LastChecked(ctx context.Context, resourceName string) (store.CheckedResource, bool, error) {
	var (
		row       store.CheckedResource
		checkedAt time.Time
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT resource_name, version_json, checked_at FROM resource_checks WHERE pipeline_id = $1 AND resource_name = $2`,
		s.pipelineID, resourceName,
	).Scan(&row.Name, &row.Version, &checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.CheckedResource{}, false, nil
	}

	if err != nil {
		return store.CheckedResource{}, false, fmt.Errorf("could not query resource_checks: %w", err)
	}

	row.CheckedAt = checkedAt.UTC()

	return row, true, nil
}

// CheckedResources lists every resource version the watcher has recorded.
func (s *Store) CheckedResources(ctx context.Context) ([]store.CheckedResource, error) {
	return collect(ctx, s.db, "resource checks",
		`SELECT resource_name, version_json, checked_at FROM resource_checks
		 WHERE pipeline_id = $1 ORDER BY resource_name`,
		[]any{s.pipelineID}, func(rows *sql.Rows) (store.CheckedResource, error) {
			var (
				row       store.CheckedResource
				checkedAt time.Time
			)

			err := rows.Scan(&row.Name, &row.Version, &checkedAt)
			row.CheckedAt = checkedAt.UTC()

			return row, err //nolint:wrapcheck // collect wraps with the thing being read
		})
}

// RecordCheckError files why a check failed, or clears the filing when the
// message is empty.
func (s *Store) RecordCheckError(ctx context.Context, resourceName, message string) error {
	if message == "" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM resource_check_errors WHERE pipeline_id = $1 AND resource_name = $2`,
			s.pipelineID, resourceName,
		)
		if err != nil {
			return fmt.Errorf("could not clear the check error for %q: %w", resourceName, err)
		}

		return nil
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO resource_check_errors (pipeline_id, resource_name, message, failed_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (pipeline_id, resource_name) DO UPDATE SET message = excluded.message, failed_at = excluded.failed_at`,
		s.pipelineID, resourceName, boundedError(message), now(),
	)
	if err != nil {
		return fmt.Errorf("could not record the check error for %q: %w", resourceName, err)
	}

	return nil
}

// CheckErrors is every resource this pipeline cannot currently check.
func (s *Store) CheckErrors(ctx context.Context) ([]store.CheckError, error) {
	return collect(ctx, s.db, "resource check errors",
		`SELECT resource_name, message, failed_at FROM resource_check_errors
		 WHERE pipeline_id = $1 ORDER BY resource_name`,
		[]any{s.pipelineID}, func(rows *sql.Rows) (store.CheckError, error) {
			var (
				row      store.CheckError
				failedAt time.Time
			)

			err := rows.Scan(&row.Name, &row.Message, &failedAt)
			row.FailedAt = failedAt.UTC()

			return row, err //nolint:wrapcheck // collect wraps with the thing being read
		})
}

// RecordPassedVersion records that jobName completed successfully against
// this exact version of a resource.
func (s *Store) RecordPassedVersion(ctx context.Context, jobName, resourceName, versionJSON, buildID string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		err := ensureVersion(ctx, tx, s.pipelineID, resourceName, versionJSON)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO job_versions (pipeline_id, job_name, resource_name, version_json, recorded_at, build_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (pipeline_id, job_name, resource_name, version_json)
			DO UPDATE SET build_id = excluded.build_id, recorded_at = excluded.recorded_at
		`, s.pipelineID, jobName, resourceName, versionJSON, now(), buildID)

		return err //nolint:wrapcheck // wrapped below with the job
	})
	if err != nil {
		return fmt.Errorf("could not record a passed version for job %q: %w", jobName, err)
	}

	return nil
}

// PassedVersions lists the resource versions a job has recorded as green.
func (s *Store) PassedVersions(ctx context.Context, jobName string, limit int) ([]store.PassedVersion, error) {
	return collect(ctx, s.db, "job versions", `
		SELECT resource_name, version_json, recorded_at
		FROM job_versions WHERE pipeline_id = $1 AND job_name = $2
		ORDER BY recorded_at DESC, seq DESC LIMIT $3
	`, []any{s.pipelineID, jobName, rowLimit(limit)}, func(rows *sql.Rows) (store.PassedVersion, error) {
		var (
			row        store.PassedVersion
			recordedAt time.Time
		)

		err := rows.Scan(&row.Resource, &row.Version, &recordedAt)
		row.RecordedAt = recordedAt.UTC()

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// HasPassedVersionSet reports whether jobName has one build in which EVERY
// (resource, version) pair in want was green at the same time. Rows with no
// build id can never satisfy it; an empty want is vacuously true.
func (s *Store) HasPassedVersionSet(ctx context.Context, jobName string, want map[string]string) (bool, error) {
	if len(want) == 0 {
		return true, nil
	}

	resources := make([]string, 0, len(want))
	versions := make([]string, 0, len(want))

	for resourceName, versionJSON := range want {
		resources = append(resources, resourceName)
		versions = append(versions, versionJSON)
	}

	var found bool

	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM job_versions
		    WHERE pipeline_id = $1 AND job_name = $2 AND build_id <> ''
		      AND (resource_name, version_json) IN (SELECT * FROM unnest($3::text[], $4::text[]))
		    GROUP BY build_id
		    HAVING COUNT(DISTINCT resource_name) = $5
		)
	`, s.pipelineID, jobName, resources, versions, len(want)).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("could not check passed versions for job %q: %w", jobName, err)
	}

	return found, nil
}

// ConsumedMark is how far a job has fanned out over a resource; zero means
// nothing taken.
func (s *Store) ConsumedMark(ctx context.Context, jobName, resourceName string) (int64, error) {
	var mark int64

	err := s.db.QueryRowContext(ctx,
		`SELECT check_order FROM job_version_cursor
		 WHERE pipeline_id = $1 AND job_name = $2 AND resource_name = $3`,
		s.pipelineID, jobName, resourceName).Scan(&mark)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("could not read the cursor for job %q: %w", jobName, err)
	}

	return mark, nil
}

// RecordConsumedMark advances a job's cursor to include order, only ever
// forward.
func (s *Store) RecordConsumedMark(ctx context.Context, jobName, resourceName string, order int64) error {
	if order <= 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_version_cursor (pipeline_id, job_name, resource_name, check_order)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (pipeline_id, job_name, resource_name)
		DO UPDATE SET check_order = GREATEST(job_version_cursor.check_order, excluded.check_order)
	`, s.pipelineID, jobName, resourceName, order)
	if err != nil {
		return fmt.Errorf("could not record the cursor for job %q: %w", jobName, err)
	}

	return nil
}

// RecordRunInput remembers that a run was created with this version, bound
// under this get.
func (s *Store) RecordRunInput(ctx context.Context, runID, inputName, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_inputs (run_id, input_name, resource_name, version_json)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, input_name) DO NOTHING
	`, runID, inputName, resourceName, versionJSON)
	if err != nil {
		return fmt.Errorf("could not record the inputs of run %q: %w", runID, err)
	}

	return nil
}

// RunInputs reports the versions a run was created with.
func (s *Store) RunInputs(ctx context.Context, runID string) ([]store.RunInput, error) {
	return collect(ctx, s.db, "the inputs of run "+runID, `
		SELECT i.input_name, i.resource_name, i.version_json FROM run_inputs i
		JOIN runs r ON r.id = i.run_id
		WHERE i.run_id = $1 AND r.pipeline_id = $2
	`, []any{runID, s.pipelineID}, func(rows *sql.Rows) (store.RunInput, error) {
		var one store.RunInput

		return one, rows.Scan(&one.Input, &one.Resource, &one.Version)
	})
}
