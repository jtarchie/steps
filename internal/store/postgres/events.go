package postgres

// run_events: the persisted side of the run-event bus (internal/events).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// AppendRunEvent persists one run event.
//
// A reader follows a run with `seq > cursor` (RunEvents), which is only safe
// if one run's events COMMIT in seq order: an identity value is taken at
// insert, not at commit, so two concurrent inserts can commit 2 before 1 and a
// reader who saw 2 has already moved past 1 for good. It holds because
// pipeline.StoreSink calls this from ONE goroutine per bus, so a run's inserts
// never overlap. That is an invariant of the caller, not something taken here
// with a lock: this is the hottest write there is. Batching or fanning out
// these inserts must keep it.
func (s *Store) AppendRunEvent(ctx context.Context, row store.RunEventRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_events
			(run_id, type, step_index, step_name, step_kind, step_id, parent_step_id,
			 status, hash, text, name, detail, duration_ms, worker, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, row.RunID, row.Type, row.StepIndex, clean(row.StepName), row.StepKind,
		row.StepID, row.ParentStepID,
		row.Status, row.Hash,
		eventText(row.Text),
		eventText(row.Name),
		eventText(row.Detail),
		row.DurationMS,
		eventText(row.Worker),
		row.At.UTC().Truncate(time.Microsecond))
	if err != nil {
		return fmt.Errorf("could not append run event for %q: %w", row.RunID, err)
	}

	return nil
}

// DeleteStepEvents removes a step's events of one type. One statement that reads nothing first, so it needs no write() lock.
func (s *Store) DeleteStepEvents(ctx context.Context, runID string, stepID int64, eventType string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM run_events e
		USING runs r
		WHERE r.id = e.run_id AND r.pipeline_id = $1
		  AND e.run_id = $2 AND e.step_id = $3 AND e.type = $4
	`, s.pipelineID, runID, stepID, eventType)
	if err != nil {
		return fmt.Errorf("could not delete %s events of step %d in %q: %w", eventType, stepID, runID, err)
	}

	return nil
}

// RunEvents replays a run's events in order, from afterSeq exclusive.
func (s *Store) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.RunEventRow, error) {
	return collect(ctx, s.db, "run events", `
		SELECT e.seq, e.run_id, e.type, e.step_index, e.step_name, e.step_kind,
		       e.step_id, e.parent_step_id,
		       e.status, e.hash, e.text, e.name, e.detail, e.duration_ms, e.worker, e.created_at
		FROM run_events e
		JOIN runs r ON r.id = e.run_id
		WHERE e.run_id = $1 AND r.pipeline_id = $2 AND e.seq > $3
		ORDER BY e.seq
		LIMIT $4
	`, []any{runID, s.pipelineID, afterSeq, rowLimit(limit)}, func(rows *sql.Rows) (store.RunEventRow, error) {
		var (
			row       store.RunEventRow
			createdAt time.Time
		)

		err := rows.Scan(&row.Seq, &row.RunID, &row.Type, &row.StepIndex,
			&row.StepName, &row.StepKind, &row.StepID, &row.ParentStepID,
			&row.Status, &row.Hash, &row.Text,
			&row.Name, &row.Detail, &row.DurationMS, &row.Worker, &createdAt)

		row.At = createdAt.UTC()

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}
