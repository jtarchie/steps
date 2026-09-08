package sqlite

// run_events: the persisted side of the run-event bus (internal/events), so a
// finished run reads back exactly as it read live.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// AppendRunEvent persists one run event. Called from the bus's sink
// goroutine (internal/events), so writes are already serialized.
func (s *Store) AppendRunEvent(ctx context.Context, row store.RunEventRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_events
			(run_id, type, step_index, step_name, step_kind, step_id, parent_step_id,
			 status, hash, text, name, detail, duration_ms, worker, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, row.RunID, row.Type, row.StepIndex, row.StepName, row.StepKind,
		row.StepID, row.ParentStepID,
		row.Status, row.Hash,
		truncateUTF8(row.Text, store.MaxEventTextBytes),
		truncateUTF8(row.Name, store.MaxEventTextBytes),
		truncateUTF8(row.Detail, store.MaxEventTextBytes),
		row.DurationMS,
		truncateUTF8(row.Worker, store.MaxEventTextBytes),
		row.At.UTC().Format(sortableNano))
	if err != nil {
		return fmt.Errorf("could not append run event for %q: %w", row.RunID, err)
	}

	return nil
}

// RunEvents replays a run's events in order, from afterSeq exclusive. Pass 0
// for the whole run — which is also how a reconnecting live view catches up
// on what it missed without re-reading what it already has.
func (s *Store) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.RunEventRow, error) {
	return collect(ctx, s.db, "run events", `
		SELECT seq, run_id, type, step_index, step_name, step_kind,
		       step_id, parent_step_id,
		       status, hash, text, name, detail, duration_ms, worker, created_at
		FROM run_events
		WHERE run_id = ? AND seq > ?
		ORDER BY seq
		LIMIT ?
	`, []any{runID, afterSeq, limit}, func(rows *sql.Rows) (store.RunEventRow, error) {
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
