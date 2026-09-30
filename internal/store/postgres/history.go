package postgres

// resource_versions: every version steps has seen of a resource, in the order
// it saw them. The semantics — what check_order means, why a run-filed row is
// re-ordered when a check first reports it, why the prune never reaches the
// reported window — are the sqlite driver's (internal/store/sqlite/history.go).

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordVersions files what a check reported, assigning each new version the
// next check_order, and prunes beyond the cap. It returns how many versions
// were new to check-history.
func (s *Store) RecordVersions(ctx context.Context, resourceName string, versions []map[string]any, limit int) (int, error) {
	if len(versions) == 0 {
		return 0, nil
	}

	if limit < 0 {
		limit = store.DefaultResourceVersionCap
	}

	encoded, err := encodeVersions(versions)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	var added int

	err = s.write(ctx, func(tx *sql.Tx) error {
		added, err = insertNewVersions(ctx, tx, s.pipelineID, resourceName, encoded)
		if err != nil {
			return err
		}

		if limit == 0 {
			return nil
		}

		floor, err := minReportedOrder(ctx, tx, s.pipelineID, resourceName, encoded)
		if err != nil {
			return err
		}

		return pruneVersions(ctx, tx, s.pipelineID, resourceName, limit, floor)
	})
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	return added, nil
}

func encodeVersions(versions []map[string]any) ([]string, error) {
	encoded := make([]string, 0, len(versions))

	for _, version := range versions {
		one, err := store.EncodeVersion(version)
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller names the resource
		}

		encoded = append(encoded, one)
	}

	return encoded, nil
}

// insertNewVersions files the versions check-history does not hold, each
// taking the next check_order in report order, and reports how many.
//
// One statement for the whole report rather than one per version: a check
// re-reports its whole window every poll, and over a network a round trip per
// version is a thousand of them a poll for nothing. The sqlite driver's loop
// numbered only the rows it changed; here ROW_NUMBER over the rows that WILL
// change does the same, so the order neither advances nor gaps on a steady
// poll. DISTINCT ON keeps a version reported twice from being two rows of one
// upsert, which Postgres refuses.
//
// The MAX it numbers from is a read before a write, which is why every
// caller holds the pipeline lock (see write).
func insertNewVersions(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string, encoded []string) (int, error) {
	result, err := tx.ExecContext(ctx, `
		WITH reported AS (
		    SELECT DISTINCT ON (version) version, position
		    FROM unnest($3::text[]) WITH ORDINALITY AS r(version, position)
		    ORDER BY version, position
		), fresh AS (
		    SELECT version, position FROM reported
		    WHERE NOT EXISTS (
		        SELECT 1 FROM resource_versions rv
		        WHERE rv.pipeline_id = $1 AND rv.resource_name = $2
		          AND rv.version_json = reported.version AND rv.from_check
		    )
		)
		INSERT INTO resource_versions (pipeline_id, resource_name, version_json, check_order, from_check)
		SELECT $1::bigint, $2::text, version,
		       (SELECT COALESCE(MAX(check_order), 0) FROM resource_versions
		        WHERE pipeline_id = $1 AND resource_name = $2)
		       + ROW_NUMBER() OVER (ORDER BY position),
		       TRUE
		FROM fresh
		ON CONFLICT (pipeline_id, resource_name, version_json)
		DO UPDATE SET from_check = TRUE, check_order = excluded.check_order
		WHERE resource_versions.from_check = FALSE
	`, pipelineID, resourceName, encoded)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	return int(changed), nil
}

// minReportedOrder is the lowest check_order among the versions a check just
// reported — the floor below which pruning is safe.
func minReportedOrder(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string, encoded []string) (int64, error) {
	var lowest sql.NullInt64

	err := tx.QueryRowContext(ctx,
		`SELECT MIN(check_order) FROM resource_versions
		 WHERE pipeline_id = $1 AND resource_name = $2 AND version_json = ANY($3::text[])`,
		pipelineID, resourceName, textArray(encoded)).Scan(&lowest)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	if !lowest.Valid {
		return 1<<62 - 1, nil
	}

	return lowest.Int64, nil
}

// pruneVersions drops the oldest versions beyond the cap, never one at or
// above floor.
func pruneVersions(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string, limit int, floor int64) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM resource_versions
		WHERE pipeline_id = $1 AND resource_name = $2 AND check_order < $3 AND check_order NOT IN (
			SELECT check_order FROM resource_versions
			WHERE pipeline_id = $1 AND resource_name = $2
			ORDER BY check_order DESC
			LIMIT $4
		)
	`, pipelineID, resourceName, floor, limit)
	if err != nil {
		return fmt.Errorf("could not prune versions for %q: %w", resourceName, err)
	}

	return nil
}

// ResourceVersionsJSON returns the versions a CHECK has reported for a
// resource, oldest first.
func (s *Store) ResourceVersionsJSON(ctx context.Context, resourceName string) ([]string, error) {
	return collect(ctx, s.db, "resource versions",
		`SELECT version_json FROM resource_versions
		 WHERE pipeline_id = $1 AND resource_name = $2 AND from_check
		 ORDER BY check_order`,
		[]any{s.pipelineID, resourceName}, scanString)
}

// VersionOrders maps every recorded version of a resource to its
// check_order, including the rows a check did not file.
func (s *Store) VersionOrders(ctx context.Context, resourceName string) (map[string]int64, error) {
	type ordered struct {
		encoded string
		order   int64
	}

	rows, err := collect(ctx, s.db, "the version order of "+resourceName,
		`SELECT version_json, check_order FROM resource_versions WHERE pipeline_id = $1 AND resource_name = $2`,
		[]any{s.pipelineID, resourceName}, func(rows *sql.Rows) (ordered, error) {
			var row ordered

			return row, rows.Scan(&row.encoded, &row.order)
		})
	if err != nil {
		return nil, err
	}

	orders := make(map[string]int64, len(rows))
	for _, row := range rows {
		orders[row.encoded] = row.order
	}

	return orders, nil
}

// RecordVersionOrder files a version if it is not already known and returns
// the order it sits at.
func (s *Store) RecordVersionOrder(ctx context.Context, resourceName, versionJSON string) (int64, error) {
	var order int64

	err := s.write(ctx, func(tx *sql.Tx) error {
		err := ensureVersion(ctx, tx, s.pipelineID, resourceName, versionJSON)
		if err != nil {
			return err
		}

		return tx.QueryRowContext(ctx,
			`SELECT check_order FROM resource_versions
			 WHERE pipeline_id = $1 AND resource_name = $2 AND version_json = $3`,
			s.pipelineID, resourceName, versionJSON).Scan(&order)
	})
	if err != nil {
		return 0, fmt.Errorf("could not record version for %q: %w", resourceName, err)
	}

	return order, nil
}

// ensureVersion records a version as seen, so a row referencing it has a
// parent. from_check stays false: USED is not the same as REPORTED.
//
// The MAX it numbers from is a read before a write; callers hold the
// pipeline lock.
func ensureVersion(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName, versionJSON string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO resource_versions (pipeline_id, resource_name, version_json, check_order, from_check)
		SELECT $1::bigint, $2::text, $3::text, COALESCE(MAX(check_order), 0) + 1, FALSE
		FROM resource_versions WHERE pipeline_id = $1 AND resource_name = $2
		ON CONFLICT (pipeline_id, resource_name, version_json) DO NOTHING
	`, pipelineID, resourceName, versionJSON)
	if err != nil {
		return fmt.Errorf("could not record version for %q: %w", resourceName, err)
	}

	return nil
}

// GreenVersions returns the versions of a resource that EVERY named upstream
// job has gone green against, oldest first.
func (s *Store) GreenVersions(ctx context.Context, resourceName string, upstreamJobs []string) ([]map[string]any, error) {
	encoded, err := collect(ctx, s.db, "green versions of "+resourceName, `
		SELECT rv.version_json FROM resource_versions rv
		WHERE rv.pipeline_id = $1 AND rv.resource_name = $2
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest($3::text[]) AS upstream(job)
		      WHERE NOT EXISTS (
		          SELECT 1 FROM job_versions jv
		          WHERE jv.pipeline_id = rv.pipeline_id AND jv.job_name = upstream.job
		            AND jv.resource_name = rv.resource_name AND jv.version_json = rv.version_json
		      )
		  )
		ORDER BY rv.check_order
	`, []any{s.pipelineID, resourceName, textArray(upstreamJobs)}, scanString)
	if err != nil {
		return nil, err
	}

	versions := make([]map[string]any, 0, len(encoded))

	for _, one := range encoded {
		version, err := store.DecodeVersion(one)
		if err != nil {
			return nil, fmt.Errorf("could not read green versions for %q: %w", resourceName, err)
		}

		versions = append(versions, version)
	}

	return versions, nil
}
