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
	// ConfigSHA is the configuration this run executed, empty for a run
	// started by a caller that loaded no pipeline file. Selected by every
	// RunRow query for the same reason ParentRunID is — see runColumns.
	ConfigSHA string
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

// revisionBySHA resolves the configuration a run says it executed, as a
// subselect rather than an id the caller looked up first.
//
// The SHA is the caller's answer because it travels with the CONFIGURATION —
// it is a field on the *config.Config the run was handed — while the row id
// is this database's business. That is the whole correction: the revision
// used to be read off the store handle at write time, minutes after the run
// took its config, so a reload in between made a run name a configuration it
// never executed.
//
// An unmatched sha resolves to NULL, which is what an empty one means too: a
// run started by a caller that loaded no pipeline file.
const revisionBySHA = `(SELECT id FROM pipeline_revisions WHERE pipeline_id = ? AND sha = ?)`

// runColumns is the one column list every RunRow query selects (runColumnsR
// is the same list for a query that aliases runs as r). scanRunRow decodes
// exactly this order.
const (
	runColumns  = `id, job_name, workspace, status, started_at, COALESCE(finished_at, ''), COALESCE(parent_run_id, ''), ` + configSHA
	runColumnsR = `r.id, r.job_name, r.workspace, r.status, r.started_at, COALESCE(r.finished_at, ''), COALESCE(r.parent_run_id, ''), ` + configSHAR
	// A subselect rather than a join, so adding the column changed no query's
	// shape: several of the reads above already join, group and alias, and a
	// second join would have had to be threaded correctly through each one
	// for a value every RunRow is contracted to carry.
	configSHA  = `COALESCE((SELECT sha FROM pipeline_revisions WHERE id = revision_id), '')`
	configSHAR = `COALESCE((SELECT sha FROM pipeline_revisions WHERE id = r.revision_id), '')`
)

// rowScanner is what both *sql.Row and *sql.Rows satisfy, so scanRunRow serves
// the single-row reads and the list queries alike.
type rowScanner interface{ Scan(dest ...any) error }

func scanRunRow(sc rowScanner) (RunRow, error) {
	var (
		row                   RunRow
		startedAt, finishedAt string
	)

	err := sc.Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt, &row.ParentRunID, &row.ConfigSHA)

	row.StartedAt = parseTimestamp(startedAt)
	row.FinishedAt = parseTimestamp(finishedAt)

	return row, err //nolint:wrapcheck // every caller names the run it was reading
}

var (
	// ErrRunExists is a MINT against an id some run already holds.
	//
	// Loud on purpose, and the whole reason StartRun is no longer an upsert.
	// runs.id is a single global primary key, and the upsert answered a
	// collision by taking the existing row OVER: a finished run flipped back
	// to running and its workspace repointed, while the row kept the old
	// job_name and the old pipeline_id — so every child row the new run wrote
	// hung off a record describing a different run of a different job. A
	// build that refuses to start is a bad afternoon; that was silent history
	// corruption, and nothing downstream could tell it had happened.
	ErrRunExists = errors.New("a run with this id already exists")
	// ErrNoSuchRun is a RESUME of a run this pipeline does not have.
	ErrNoSuchRun = errors.New("no run with this id")
)

// StartRun records a NEW run and the workspace its steps will share.
//
// An insert, never an upsert: minting an id and continuing a run are
// different acts, and the single statement that served both could not tell
// them apart. Continuing one is ResumeRun.
//
// DO NOTHING plus a row count rather than letting the constraint violation
// out, so the answer does not depend on how a driver spells its unique-index
// error.
func (s *Store) StartRun(ctx context.Context, id, jobName, workspaceDir, configSHA string) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, pipeline_id, job_name, workspace, status, started_at, revision_id)
		VALUES (?, ?, ?, ?, 'running', ?, `+revisionBySHA+`)
		ON CONFLICT (id) DO NOTHING
	`, id, s.pipelineID, jobName, workspaceDir, nowNano(), s.pipelineID, configSHA)
	if err != nil {
		return fmt.Errorf("could not record run %q: %w", id, err)
	}

	// Deliberately NOT scoped to this pipeline: runs.id is global across every
	// pipeline in the state file, so a scoped check would report success and
	// then write this run's events onto another pipeline's row.
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not record run %q: %w", id, err)
	}

	if inserted == 0 {
		return fmt.Errorf("%w: %q", ErrRunExists, id)
	}

	return nil
}

// ResumeRun puts an existing run back in flight and points it at the build
// this attempt is using.
//
// The legitimate half of what the old upsert did. It updates and never
// inserts, so a resume of a run that is not there is an error instead of a
// silently invented row — and it is scoped to this pipeline, because a run id
// names a row in ONE pipeline and reaching another one would put a foreign
// run back in flight under this pipeline name.
//
// job_name is deliberately not written: the job a run belongs to was decided
// when it was minted, and a resume that could rewrite it would make the run
// history disagree with the events already recorded against it.
//
// The REVISION is written, and for the mirror of that reason: a resume
// continues a failed run under the configuration it is being resumed with,
// which is usually the one that fixed it. Leaving the original would make the
// run claim it executed a pipeline that nothing in it ever ran.
//
// COALESCEd rather than assigned, because a subselect that matches nothing is
// not an answer: an empty sha (a caller that loaded no file) or a row this
// pipeline no longer has would ERASE a configuration the run had correctly
// recorded, turning "it ran that" into "it ran none" — the one thing this
// column exists to deny. An update that cannot name a new configuration keeps
// the old one.
func (s *Store) ResumeRun(ctx context.Context, id, workspaceDir, configSHA string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = 'running', workspace = ?,
		                revision_id = COALESCE(`+revisionBySHA+`, revision_id)
		WHERE id = ? AND pipeline_id = ?
	`, workspaceDir, s.pipelineID, configSHA, id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not resume run %q: %w", id, err)
	}

	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not resume run %q: %w", id, err)
	}

	if updated == 0 {
		return fmt.Errorf("%w: %q", ErrNoSuchRun, id)
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

	// Joined to runs for the pipeline, which run_steps has no column of its
	// own for. Run ids are minted without a uniqueness check, so an unscoped
	// read here would hand one run another's completed steps and --resume
	// would skip work it never did.
	steps, err := collect(ctx, s.db, "the steps of run "+runID,
		`SELECT s.step_index, s.step_name FROM run_steps s
		 JOIN runs r ON r.id = s.run_id
		 WHERE s.run_id = ? AND r.pipeline_id = ?`,
		[]any{runID, s.pipelineID}, func(rows *sql.Rows) (step, error) {
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
