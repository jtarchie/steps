package postgres

// runs: one row per invocation — what --resume continues and the history
// views list.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// runColumns is the one column list every RunRow query selects (runColumnsR
// is the same list for a query aliasing runs as r); scanRunRow decodes it.
const (
	runColumns  = `id, job_name, workspace, status, started_at, finished_at, COALESCE(parent_run_id, ''), ` + configSHA + `, COALESCE(rerun_of, ''), COALESCE(rerun_of_build, 0)`
	runColumnsR = `r.id, r.job_name, r.workspace, r.status, r.started_at, r.finished_at, COALESCE(r.parent_run_id, ''), ` + configSHAR + `, COALESCE(r.rerun_of, ''), COALESCE(r.rerun_of_build, 0)`
	configSHA   = `COALESCE((SELECT sha FROM pipeline_revisions WHERE id = revision_id), '')`
	configSHAR  = `COALESCE((SELECT sha FROM pipeline_revisions WHERE id = r.revision_id), '')`
)

type rowScanner interface{ Scan(dest ...any) error }

func scanRunRow(sc rowScanner) (store.RunRow, error) {
	var (
		row      store.RunRow
		started  time.Time
		finished sql.NullTime
	)

	err := sc.Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &started, &finished, &row.ParentRunID, &row.ConfigSHA, &row.RerunOf, &row.RerunOfBuild)

	row.StartedAt = started.UTC()
	row.FinishedAt = utc(finished)

	return row, err //nolint:wrapcheck // every caller names the run it was reading
}

func scanRunRowFrom(rows *sql.Rows) (store.RunRow, error) { return scanRunRow(rows) }

// StartRun records a NEW run; an id already present anywhere in the schema
// is store.ErrRunExists, since run ids are global.
func (s *Store) StartRun(ctx context.Context, id, jobName, workspaceDir, configSHA string) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, pipeline_id, job_name, workspace, status, started_at, revision_id)
		VALUES ($1, $2, $3, $4, 'running', $5,
		        (SELECT id FROM pipeline_revisions WHERE pipeline_id = $2 AND sha = $6))
		ON CONFLICT (id) DO NOTHING
	`, id, s.pipelineID, jobName, clean(workspaceDir), now(), configSHA)
	if err != nil {
		return fmt.Errorf("could not record run %q: %w", id, err)
	}

	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not record run %q: %w", id, err)
	}

	if inserted == 0 {
		return fmt.Errorf("%w: %q", store.ErrRunExists, id)
	}

	return nil
}

// ResumeRun puts an existing run of this pipeline back in flight, keeping its
// recorded configuration when the sha names none.
func (s *Store) ResumeRun(ctx context.Context, id, workspaceDir, configSHA string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'running', workspace = $1,
		       revision_id = COALESCE((SELECT id FROM pipeline_revisions WHERE pipeline_id = $2 AND sha = $3), revision_id)
		WHERE id = $4 AND pipeline_id = $2
	`, clean(workspaceDir), s.pipelineID, configSHA, id)
	if err != nil {
		return fmt.Errorf("could not resume run %q: %w", id, err)
	}

	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not resume run %q: %w", id, err)
	}

	if updated == 0 {
		return fmt.Errorf("%w: %q", store.ErrNoSuchRun, id)
	}

	return nil
}

// FinishRun records how a run ended, and when.
func (s *Store) FinishRun(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = $1, finished_at = $2 WHERE id = $3 AND pipeline_id = $4`,
		status, now(), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not finish run %q: %w", id, err)
	}

	return nil
}

// RecordRunStep marks one step of one build of a run as done.
func (s *Store) RecordRunStep(ctx context.Context, runID, buildID string, index int, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_steps (run_id, build_id, step_index, step_name) VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, build_id, step_index) DO NOTHING
	`, runID, buildID, index, name)
	if err != nil {
		return fmt.Errorf("could not record step %d of build %q: %w", index, buildID, err)
	}

	return nil
}

// RecordRunParent notes that a run was forked from another by a replay.
func (s *Store) RecordRunParent(ctx context.Context, runID, parentID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET parent_run_id = $1 WHERE id = $2 AND pipeline_id = $3`, parentID, runID, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not record the parent of run %q: %w", runID, err)
	}

	return nil
}

// RecordRunRerun notes which build of which run a retry re-ran.
func (s *Store) RecordRunRerun(ctx context.Context, runID, originalID string, build int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET rerun_of = $1, rerun_of_build = $2 WHERE id = $3 AND pipeline_id = $4`, originalID, build, runID, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not record what run %q reran: %w", runID, err)
	}

	return nil
}

// CompletedRunSteps returns the steps a run already finished, in the order
// they finished.
func (s *Store) CompletedRunSteps(ctx context.Context, runID string) ([]store.RunStep, error) {
	return collect(ctx, s.db, "the steps of run "+runID,
		`SELECT s.build_id, s.step_index, s.step_name FROM run_steps s
		 JOIN runs r ON r.id = s.run_id
		 WHERE s.run_id = $1 AND r.pipeline_id = $2
		 ORDER BY s.seq`,
		[]any{runID, s.pipelineID}, func(rows *sql.Rows) (store.RunStep, error) {
			var one store.RunStep

			return one, rows.Scan(&one.BuildID, &one.Index, &one.Name)
		})
}

// ListRuns returns run invocations, newest first. An empty jobName covers
// every job.
func (s *Store) ListRuns(ctx context.Context, jobName string, limit int) ([]store.RunRow, error) {
	filter, args := byJob(jobName, []any{s.pipelineID})

	return collect(ctx, s.db, "runs", `
		SELECT `+runColumns+`
		FROM runs
		WHERE pipeline_id = $1`+filter+`
		ORDER BY started_at DESC, seq DESC
		LIMIT `+next(args), append(args, rowLimit(limit)), scanRunRowFrom)
}

// LatestRunByJob returns the most recent run for every job that has one,
// counting a rerun only when it reruns the job's latest other run.
func (s *Store) LatestRunByJob(ctx context.Context) (map[string]store.RunRow, error) {
	rows, err := collect(ctx, s.db, "latest runs", `
		SELECT `+runColumns+` FROM (
		    SELECT *, ROW_NUMBER() OVER (PARTITION BY job_name ORDER BY started_at DESC, seq DESC) AS recency
		    FROM runs
		    WHERE pipeline_id = $1
		      AND (rerun_of IS NULL OR rerun_of IN (
		          SELECT id FROM (
		              SELECT id, ROW_NUMBER() OVER (PARTITION BY job_name ORDER BY started_at DESC, seq DESC) AS recency
		              FROM runs WHERE pipeline_id = $1 AND rerun_of IS NULL
		          ) AS originals WHERE recency = 1
		      ))
		) AS latest WHERE recency = 1
	`, []any{s.pipelineID}, scanRunRowFrom)
	if err != nil {
		return nil, err
	}

	latest := make(map[string]store.RunRow, len(rows))
	for _, row := range rows {
		latest[row.JobName] = row
	}

	return latest, nil
}

// RunsUsingNode lists the runs whose events reference a node hash.
func (s *Store) RunsUsingNode(ctx context.Context, hash string, limit int) ([]store.RunRow, error) {
	return collect(ctx, s.db, "runs using node", `
		SELECT `+runColumnsR+`
		FROM runs r
		WHERE r.pipeline_id = $1
		  AND r.id IN (SELECT DISTINCT run_id FROM run_events WHERE hash = $2)
		ORDER BY r.started_at DESC, r.seq DESC
		LIMIT $3
	`, []any{s.pipelineID, hash, rowLimit(limit)}, scanRunRowFrom)
}

// FindRunRow reads one run of this pipeline by id.
func (s *Store) FindRunRow(ctx context.Context, id string) (store.RunRow, bool, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1 AND pipeline_id = $2`,
		fmt.Sprintf("could not read run %q", id), id, s.pipelineID)
}

// FirstRunSince returns the oldest run of a job started at or after since.
func (s *Store) FirstRunSince(ctx context.Context, jobName string, since time.Time) (store.RunRow, bool, error) {
	return s.oneRun(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE pipeline_id = $1 AND job_name = $2 AND started_at >= $3
		ORDER BY started_at, seq
		LIMIT 1
	`, fmt.Sprintf("could not look for a run of %q", jobName),
		s.pipelineID, jobName, since.UTC().Truncate(time.Microsecond))
}

func (s *Store) oneRun(ctx context.Context, query, what string, args ...any) (store.RunRow, bool, error) {
	row, err := scanRunRow(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return store.RunRow{}, false, nil
	}

	if err != nil {
		return store.RunRow{}, false, fmt.Errorf("%s: %w", what, err)
	}

	return row, true, nil
}
