package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// RecordActivity appends an entry under this pipeline and trims the whole log; see the sqlite driver for why the name is copied.
func (s *Store) RecordActivity(ctx context.Context, entry store.ActivityEntry, limit int) error {
	detail := entry.Detail
	if detail == "" {
		detail = "{}"
	}

	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO activity (pipeline_id, pipeline, actor, action, target, detail, at)
			VALUES ($1, (SELECT name FROM pipelines WHERE id = $1), $2, $3, $4, $5, $6)
		`, s.pipelineID, clean(entry.Actor), clean(entry.Action), clean(entry.Target), clean(detail), now())
		if err != nil || limit <= 0 {
			return err //nolint:wrapcheck // wrapped below
		}

		_, err = tx.ExecContext(ctx, `
			DELETE FROM activity WHERE id <= (SELECT id FROM activity ORDER BY id DESC LIMIT 1 OFFSET $1)
		`, limit)

		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("could not record activity: %w", err)
	}

	return nil
}

// Activity reads the log newest first; see the sqlite driver for the pipeline filter.
func (r *Reader) Activity(ctx context.Context, filter store.ActivityFilter) ([]store.ActivityEntry, error) {
	return collect(ctx, r.db, "activity", `
		SELECT id, pipeline, actor, action, target, detail, at
		FROM activity
		WHERE ($1 = '' OR pipeline = $1 OR pipeline_id = (SELECT id FROM pipelines WHERE name = $1))
		  AND ($2 = '' OR actor = $2)
		  AND ($3 = '' OR action = $3)
		  AND ($4 = '' OR target = $4)
		  AND ($5::timestamptz IS NULL OR at >= $5)
		  AND ($6 = 0 OR id < $6)
		  AND (cardinality($8::text[]) = 0 OR action = ANY($8::text[]))
		ORDER BY id DESC
		LIMIT $7
	`, []any{filter.Pipeline, filter.Actor, filter.Action, filter.Target, sinceArg(filter.Since), filter.Before, rowLimit(filter.Limit), nonNilStrings(filter.Actions)}, func(rows *sql.Rows) (store.ActivityEntry, error) {
		var (
			entry store.ActivityEntry
			at    sql.NullTime
		)

		err := rows.Scan(&entry.ID, &entry.Pipeline, &entry.Actor, &entry.Action, &entry.Target, &entry.Detail, &at)
		entry.At = utc(at)

		return entry, err //nolint:wrapcheck // collect names what it was reading
	})
}

// sinceArg is a zero time as NULL, which the query reads as no bound.
func sinceArg(since time.Time) any {
	if since.IsZero() {
		return nil
	}

	return since.UTC()
}

// nonNilStrings is an empty array rather than NULL for a list nobody gave.
func nonNilStrings(list []string) []string {
	if list == nil {
		return []string{}
	}

	return list
}
