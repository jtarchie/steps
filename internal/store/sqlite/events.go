package sqlite

// run_events: the persisted side of the run-event bus (internal/events), so a
// finished run reads back exactly as it read live.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// AppendRunEvent persists one run event. Called from the bus's sink
// goroutine (internal/events), so writes are already serialized. The EXISTS guard is one more primary-key probe of runs, the same one the foreign key already pays.
func (s *Store) AppendRunEvent(ctx context.Context, row store.RunEventRow) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO run_events
			(run_id, type, step_index, step_name, step_kind, step_id, parent_step_id,
			 status, hash, text, name, detail, duration_ms, worker, created_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM runs WHERE id = ? AND pipeline_id = ?)
	`, row.RunID, row.Type, row.StepIndex, row.StepName, row.StepKind,
		row.StepID, row.ParentStepID,
		row.Status, row.Hash,
		store.TruncateUTF8(row.Text, store.MaxEventTextBytes),
		store.TruncateUTF8(row.Name, store.MaxEventTextBytes),
		store.TruncateUTF8(row.Detail, store.MaxEventTextBytes),
		row.DurationMS,
		store.TruncateUTF8(row.Worker, store.MaxEventTextBytes),
		row.At.UTC().Format(sortableNano), row.RunID, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not append run event for %q: %w", row.RunID, err)
	}

	return s.requireRun(ctx, result, row.RunID)
}

// DeleteStepEvents removes a step's events of one type. Called from the sink goroutine ahead of the record that replaces them, so the two never interleave with a chunk still queued.
//
// Scoped by EXISTS on the one run rather than IN over the pipeline's runs: this runs once per step output, and the IN form built the pipeline's whole run list to answer for one id.
func (s *Store) DeleteStepEvents(ctx context.Context, runID string, stepID int64, eventType string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM run_events
		WHERE run_id = ? AND step_id = ? AND type = ?
		  AND EXISTS (SELECT 1 FROM runs WHERE id = run_events.run_id AND pipeline_id = ?)
	`, runID, stepID, eventType, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not delete %s events of step %d in %q: %w", eventType, stepID, runID, err)
	}

	return nil
}

// MaxStepID is joined to runs for the pipeline, as RunEvents is.
func (s *Store) MaxStepID(ctx context.Context, runID string) (int64, error) {
	var id int64

	err := s.reads.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(e.step_id), 0)
		FROM run_events e
		JOIN runs r ON r.id = e.run_id
		WHERE e.run_id = ? AND r.pipeline_id = ?
	`, runID, s.pipelineID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("could not read the step ids of run %q: %w", runID, err)
	}

	return id, nil
}

// RunEvents replays a run's events in order, from afterSeq exclusive. Pass 0
// for the whole run — which is also how a reconnecting live view catches up
// on what it missed without re-reading what it already has.
//
// Joined to runs for the pipeline, which run_events has no column of its own
// for: a holder of this facet alone has no FindRunRow to ask first, so a run
// id another pipeline minted must read as nothing here rather than as that
// pipeline's events.
func (s *Store) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.RunEventRow, error) {
	return collect(ctx, s.reads, "run events", `
		SELECT e.seq, e.run_id, e.type, e.step_index, e.step_name, e.step_kind,
		       e.step_id, e.parent_step_id,
		       e.status, e.hash, e.text, e.name, e.detail, e.duration_ms, e.worker, e.created_at
		FROM run_events e
		JOIN runs r ON r.id = e.run_id
		WHERE e.run_id = ? AND r.pipeline_id = ? AND e.seq > ?
		ORDER BY e.seq
		LIMIT ?
	`, []any{runID, s.pipelineID, afterSeq, rowLimit(limit)}, func(rows *sql.Rows) (store.RunEventRow, error) {
		var (
			row       store.RunEventRow
			createdAt string
		)

		err := rows.Scan(&row.Seq, &row.RunID, &row.Type, &row.StepIndex,
			&row.StepName, &row.StepKind, &row.StepID, &row.ParentStepID,
			&row.Status, &row.Hash, &row.Text,
			&row.Name, &row.Detail, &row.DurationMS, &row.Worker, &createdAt)

		row.At = parseTimestamp(createdAt)

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// AppendStepOutput stores one piece of a kept output; the EXISTS guard is AppendRunEvent's, so another pipeline's run reads as no run.
func (s *Store) AppendStepOutput(ctx context.Context, runID string, stepID int64, n, seq int, chunk []byte) error {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO run_step_outputs (run_id, step_id, n, seq, chunk)
		SELECT ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM runs WHERE id = ? AND pipeline_id = ?)
	`, runID, stepID, n, seq, chunk, runID, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not append output %d of step %d in %q: %w", n, stepID, runID, err)
	}

	return s.requireRun(ctx, result, runID)
}

// StepOutput is one query per piece, so no connection is held for as long as the reader takes: a slow browser pulling 64 MiB would otherwise sit on one.
func (s *Store) StepOutput(ctx context.Context, runID string, stepID int64, n int, yield func(chunk []byte) error) (bool, error) {
	found := false

	for seq := -1; ; {
		var chunk []byte

		err := s.reads.QueryRowContext(ctx, `
			SELECT o.seq, o.chunk
			FROM run_step_outputs o
			JOIN runs r ON r.id = o.run_id
			WHERE o.run_id = ? AND r.pipeline_id = ? AND o.step_id = ? AND o.n = ? AND o.seq > ?
			ORDER BY o.seq
			LIMIT 1
		`, runID, s.pipelineID, stepID, n, seq).Scan(&seq, &chunk)
		if errors.Is(err, sql.ErrNoRows) {
			return found, nil
		}

		if err != nil {
			return found, fmt.Errorf("could not read output %d of step %d in %q: %w", n, stepID, runID, err)
		}

		found = true

		err = yield(chunk)
		if err != nil {
			return found, fmt.Errorf("could not hand on output %d of step %d in %q: %w", n, stepID, runID, err)
		}
	}
}
