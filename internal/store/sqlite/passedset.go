package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/store"
)

// PassedSet is the newest versions satisfying every passed: constraint at once; see store.ResolvePassedSet.
func (s *Store) PassedSet(ctx context.Context, constraints map[string][]string) (map[string]store.VersionKey, bool, error) {
	return store.ResolvePassedSet(ctx, constraints, s.passedBuilds) //nolint:wrapcheck // passedBuilds names the job it read
}

// passedBuilds is one page of a job's green builds holding every queried resource, newest first by each unfixed resource's discovery order in name order. A build holding several versions of a resource offers its newest, which is the last one a build put.
func (s *Store) passedBuilds(ctx context.Context, query store.PassedBuildsQuery) ([]map[string]store.VersionKey, error) {
	fixed, err := json.Marshal(query.Fixed)
	if err != nil {
		return nil, fmt.Errorf("could not read the passed builds of job %q: %w", query.Job, err)
	}

	args := []any{s.pipelineID, query.Job, jsonList(query.Resources), string(fixed)}

	var newest strings.Builder

	for _, resource := range query.Resources {
		if _, ok := query.Fixed[resource]; ok {
			continue
		}

		newest.WriteString("MAX(CASE WHEN resource_name = ? THEN check_order END) DESC, ")

		args = append(args, resource)
	}

	args = append(args, len(query.Resources), query.Limit, query.Offset)

	rows, err := collect(ctx, s.reads, "the passed builds of job "+query.Job, `
		WITH held AS (
		    SELECT jv.build_id, jv.resource_name, jv.version_json, rv.check_order,
		           ROW_NUMBER() OVER (PARTITION BY jv.build_id, jv.resource_name ORDER BY rv.check_order DESC) AS nth
		    FROM job_versions jv
		    JOIN resource_versions rv ON rv.pipeline_id = jv.pipeline_id
		        AND rv.resource_name = jv.resource_name AND rv.version_json = jv.version_json
		    WHERE jv.pipeline_id = ? AND jv.job_name = ? AND jv.build_id <> ''
		      AND jv.resource_name IN (SELECT value FROM json_each(?))
		      AND NOT EXISTS (
		          SELECT 1 FROM json_each(?) fixed
		          WHERE fixed.key = jv.resource_name AND fixed.value <> jv.version_json
		      )
		), builds AS (
		    SELECT build_id, ROW_NUMBER() OVER (ORDER BY `+newest.String()+`build_id DESC) AS rank
		    FROM held WHERE nth = 1
		    GROUP BY build_id HAVING COUNT(*) = ?
		    ORDER BY rank LIMIT ? OFFSET ?
		)
		SELECT h.build_id, h.resource_name, h.version_json
		FROM held h JOIN builds b ON b.build_id = h.build_id
		WHERE h.nth = 1
		ORDER BY b.rank
	`, args, func(rows *sql.Rows) ([3]string, error) {
		var row [3]string

		return row, rows.Scan(&row[0], &row[1], &row[2])
	})
	if err != nil {
		return nil, err
	}

	return store.GroupPassedBuilds(rows), nil
}

// NewestVersion is the newest version a check filed for the resource.
func (s *Store) NewestVersion(ctx context.Context, resourceName string) (store.VersionKey, bool, error) {
	var version store.VersionKey

	err := s.reads.QueryRowContext(ctx,
		`SELECT version_json FROM resource_versions
		 WHERE pipeline_id = ? AND resource_name = ? AND from_check = 1
		 ORDER BY check_order DESC LIMIT 1`,
		s.pipelineID, resourceName).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("could not read the newest version of %q: %w", resourceName, err)
	}

	return version, true, nil
}
