package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordActivity appends an entry under this pipeline, copying the pipeline's current name so the row still reads after a destroy nulls the id.
func (s *Store) RecordActivity(ctx context.Context, entry store.ActivityEntry, limit int) error {
	detail := entry.Detail
	if detail == "" {
		detail = "{}"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not record activity: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO activity (pipeline_id, pipeline, actor, action, target, detail, at)
		VALUES (?, (SELECT name FROM pipelines WHERE id = ?), ?, ?, ?, ?, ?)
	`, s.pipelineID, s.pipelineID, entry.Actor, entry.Action, entry.Target, detail, nowNano())
	if err != nil {
		return fmt.Errorf("could not record activity: %w", err)
	}

	if limit > 0 {
		_, err = tx.ExecContext(ctx, `
			DELETE FROM activity WHERE id <= (SELECT id FROM activity ORDER BY id DESC LIMIT 1 OFFSET ?)
		`, limit)
		if err != nil {
			return fmt.Errorf("could not trim activity: %w", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not record activity: %w", err)
	}

	return nil
}

// Activity reads the log newest first. A pipeline filter matches the name an entry was recorded under or, through the id, the name that pipeline has now.
func (r *Reader) Activity(ctx context.Context, filter store.ActivityFilter) ([]store.ActivityEntry, error) {
	query := `
		SELECT id, pipeline, actor, action, target, detail, at
		FROM activity
		WHERE (? = '' OR pipeline = ? OR pipeline_id = (SELECT id FROM pipelines WHERE name = ?))
		  AND (? = '' OR actor = ?)
		  AND (? = '' OR action = ?)
		  AND (? = 0 OR id < ?)
		ORDER BY id DESC
		LIMIT ?
	`
	args := []any{
		filter.Pipeline, filter.Pipeline, filter.Pipeline,
		filter.Actor, filter.Actor,
		filter.Action, filter.Action,
		filter.Before, filter.Before,
		rowLimit(filter.Limit),
	}

	return collect(ctx, r.db, "activity", query, args, func(rows *sql.Rows) (store.ActivityEntry, error) {
		var (
			entry store.ActivityEntry
			at    string
		)

		err := rows.Scan(&entry.ID, &entry.Pipeline, &entry.Actor, &entry.Action, &entry.Target, &entry.Detail, &at)
		entry.At = parseTimestamp(at)

		return entry, err //nolint:wrapcheck // collect names what it was reading
	})
}
