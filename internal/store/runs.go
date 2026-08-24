package store

// runs: one row per invocation. It is what --resume continues and what the
// history views list — distinct from job_runs, which is a cache index and has
// no row at all for a successful run containing a put or an agent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Run is one `steps run` invocation that can be resumed.
type Run struct {
	ID        string
	JobName   string
	Workspace string
	Status    string
	StartedAt string
}

// RunRow is one run invocation as the history views read it: the resume
// record plus the finish timestamp that makes a duration answerable.
type RunRow struct {
	ID         string
	JobName    string
	Workspace  string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	// ParentRunID is the run a replay forked from, empty for an ordinary run.
	// It is what lets a tuning session read as a session rather than as a pile
	// of unrelated runs that happen to share a job.
	//
	// Filled by EVERY query that builds a RunRow, deliberately: it was briefly
	// selected by only one of them, which made Replayed() answer differently
	// depending on which call site had loaded the row — the jobs list said a
	// forked run was ordinary while its own page linked its parent. That is
	// what runColumns below exists to make impossible.
	ParentRunID string
}

// Replayed reports a run forked from another by --replay.
func (r RunRow) Replayed() bool { return r.ParentRunID != "" }

// Duration is how long the run took, or how long it has been going when it
// has not finished. Zero when the run never started.
func (r RunRow) Duration() time.Duration {
	if r.StartedAt.IsZero() {
		return 0
	}

	if r.FinishedAt.IsZero() {
		return time.Since(r.StartedAt)
	}

	return r.FinishedAt.Sub(r.StartedAt)
}

// runColumns is the one column list every RunRow query selects (runColumnsR
// is the same list for a query that aliases runs as r). scanRunRow decodes
// exactly this order.
const (
	runColumns  = `id, job_name, workspace, status, started_at, COALESCE(finished_at, ''), COALESCE(parent_run_id, '')`
	runColumnsR = `r.id, r.job_name, r.workspace, r.status, r.started_at, COALESCE(r.finished_at, ''), COALESCE(r.parent_run_id, '')`
)

// rowScanner is what both *sql.Row and *sql.Rows satisfy, so scanRunRow serves
// the single-row reads and the list queries alike.
type rowScanner interface{ Scan(dest ...any) error }

func scanRunRow(sc rowScanner) (RunRow, error) {
	var (
		row                   RunRow
		startedAt, finishedAt string
	)

	err := sc.Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt, &row.ParentRunID)

	row.StartedAt = parseTimestamp(startedAt)
	row.FinishedAt = parseTimestamp(finishedAt)

	return row, err //nolint:wrapcheck // every caller names the run it was reading
}

// StartRun records a run and the workspace its steps will share.
func (s *Store) StartRun(ctx context.Context, id, jobName, workspaceDir string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, pipeline_id, job_name, workspace, status, started_at)
		VALUES (?, ?, ?, ?, 'running', ?)
		ON CONFLICT (id) DO UPDATE SET status = 'running', workspace = excluded.workspace
	`, id, s.pipelineID, jobName, workspaceDir, nowNano())
	if err != nil {
		return fmt.Errorf("could not record run %q: %w", id, err)
	}

	return nil
}

// FinishRun records how a run ended, and when. The timestamp is what makes a
// run's duration answerable at all: started_at alone leaves every finished
// run looking like it is still going.
//
// workspace is deliberately NOT cleared here, though it looks like dead weight
// once a run ends — an absolute path to a temporary directory, about ninety bytes
// a run, kept forever. It was tried and reverted: the column is read AFTER a run
// finishes by both of the features built on that, and neither can be told apart
// from here. --resume continues a FAILED run from the tree deliberately left on
// disk, and --replay forks a SUCCEEDED one that was kept with --keep-workspace.
// Clearing it on failure breaks resume; clearing it on success breaks replay.
func (s *Store) FinishRun(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ? AND pipeline_id = ?`,
		status, nowNano(), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not finish run %q: %w", id, err)
	}

	return nil
}

// RecordRunStep marks one step of a run as done, so a resume skips it.
func (s *Store) RecordRunStep(ctx context.Context, runID string, index int, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_steps (run_id, step_index, step_name) VALUES (?, ?, ?)
		ON CONFLICT (run_id, step_index) DO NOTHING
	`, runID, index, name)
	if err != nil {
		return fmt.Errorf("could not record step %d of run %q: %w", index, runID, err)
	}

	return nil
}

// RecordRunParent notes that a run was forked from another by a replay.
//
// Kept as its own statement rather than a StartRun parameter: every existing
// caller of StartRun records an ordinary run, and threading an almost-always
// empty argument through them would put replay's vocabulary in the path of
// code that has nothing to do with it.
func (s *Store) RecordRunParent(ctx context.Context, runID, parentID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET parent_run_id = ? WHERE id = ? AND pipeline_id = ?`, parentID, runID, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not record the parent of run %q: %w", runID, err)
	}

	return nil
}

// FindRun reads a run in the resume shape, which predates finished_at and is
// what --resume needs.
//
// Scoped like everything else, and here that is load-bearing rather than
// uniform: run ids are globally unique (pipeline.NewRunID is random), so in a
// shared state file `steps run a.yml --resume <id>` would otherwise happily
// continue a run of b.yml — reusing another pipeline's workspace and step
// indexes against this pipeline's plan. The id not being found is the right
// answer, and the message says which pipeline was asked.
func (s *Store) FindRun(ctx context.Context, id string) (Run, error) {
	var run Run

	err := s.db.QueryRowContext(ctx,
		`SELECT id, job_name, workspace, status, started_at FROM runs WHERE id = ? AND pipeline_id = ?`,
		id, s.pipelineID).
		Scan(&run.ID, &run.JobName, &run.Workspace, &run.Status, &run.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("no run %q was recorded for pipeline %q", id, s.pipeline)
	}

	if err != nil {
		return Run{}, fmt.Errorf("could not read run %q: %w", id, err)
	}

	return run, nil
}

// CompletedRunSteps returns the step indexes a run already finished.
func (s *Store) CompletedRunSteps(ctx context.Context, runID string) (map[int]string, error) {
	type step struct {
		index int
		name  string
	}

	steps, err := collect(ctx, s.db, "the steps of run "+runID,
		`SELECT step_index, step_name FROM run_steps WHERE run_id = ?`,
		[]any{runID}, func(rows *sql.Rows) (step, error) {
			var one step

			return one, rows.Scan(&one.index, &one.name)
		})
	if err != nil {
		return nil, err
	}

	done := make(map[int]string, len(steps))
	for _, one := range steps {
		done[one.index] = one.name
	}

	return done, nil
}

// ListRuns returns run invocations, newest first. An empty jobName covers
// every job.
func (s *Store) ListRuns(ctx context.Context, jobName string, limit int) ([]RunRow, error) {
	return collect(ctx, s.db, "runs", `
		SELECT `+runColumns+`
		FROM runs
		WHERE pipeline_id = ? AND (? = '' OR job_name = ?)
		ORDER BY started_at DESC, rowid DESC
		LIMIT ?
	`, []any{s.pipelineID, jobName, jobName, limit}, scanRunRowFrom)
}

// LatestRunByJob returns the most recent run for every job that has one,
// keyed by job name — one query for a jobs board rather than one per job.
func (s *Store) LatestRunByJob(ctx context.Context) (map[string]RunRow, error) {
	rows, err := collect(ctx, s.db, "latest runs", `
		SELECT `+runColumnsR+`
		FROM runs r
		JOIN (SELECT job_name, MAX(started_at) AS latest FROM runs
		      WHERE pipeline_id = ? GROUP BY job_name) m
		  ON m.job_name = r.job_name AND m.latest = r.started_at
		WHERE r.pipeline_id = ?
	`, []any{s.pipelineID, s.pipelineID}, scanRunRowFrom)
	if err != nil {
		return nil, err
	}

	latest := map[string]RunRow{}

	for _, row := range rows {
		// Two runs of one job can share a started_at second, so the join can
		// yield both; keep whichever sorts later by id for a stable answer.
		if prior, ok := latest[row.JobName]; ok && prior.ID > row.ID {
			continue
		}

		latest[row.JobName] = row
	}

	return latest, nil
}

// RunsUsingNode lists the runs whose events reference a node hash — the
// "which runs reused this cached step" answer a node page is built on.
func (s *Store) RunsUsingNode(ctx context.Context, hash string, limit int) ([]RunRow, error) {
	return collect(ctx, s.db, "runs using node", `
		SELECT `+runColumnsR+`
		FROM runs r
		WHERE r.pipeline_id = ?
		  AND r.id IN (SELECT DISTINCT run_id FROM run_events WHERE hash = ?)
		ORDER BY r.started_at DESC
		LIMIT ?
	`, []any{s.pipelineID, hash, limit}, scanRunRowFrom)
}

// FindRunRow reads one run in the history shape, with its finish timestamp —
// so a single run's page and the run list render from identical data.
func (s *Store) FindRunRow(ctx context.Context, id string) (RunRow, bool, error) {
	return s.oneRun(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ? AND pipeline_id = ?`,
		fmt.Sprintf("could not read run %q", id), id, s.pipelineID)
}

// FirstRunSince returns the oldest run of a job started at or after `since`,
// with ok reporting whether one exists yet.
//
// It backs the trigger-and-follow handoff: a queued job has no run id until
// the worker claims it and RunJob mints one, so the browser asks this until
// the run it caused appears. Oldest-first rather than newest, so a burst of
// triggers hands each caller the run its own click produced.
func (s *Store) FirstRunSince(ctx context.Context, jobName string, since time.Time) (RunRow, bool, error) {
	return s.oneRun(ctx, `
		SELECT `+runColumns+`
		FROM runs
		WHERE pipeline_id = ? AND job_name = ? AND started_at >= ?
		ORDER BY started_at, rowid
		LIMIT 1
	`, fmt.Sprintf("could not look for a run of %q", jobName),
		s.pipelineID, jobName, since.UTC().Format(sortableNano))
}

// oneRun reads a single RunRow, reporting ok=false rather than an error when
// there is none — both callers want "not yet" to be an ordinary answer.
func (s *Store) oneRun(ctx context.Context, query, what string, args ...any) (RunRow, bool, error) {
	row, err := scanRunRow(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return RunRow{}, false, nil
	}

	if err != nil {
		return RunRow{}, false, fmt.Errorf("%s: %w", what, err)
	}

	return row, true, nil
}

func scanRunRowFrom(rows *sql.Rows) (RunRow, error) { return scanRunRow(rows) }
