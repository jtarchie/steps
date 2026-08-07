package store

// The run-scoped key/value store: what one agent step records with
// set_context, for the steps after it to read.

import (
	"context"
	"fmt"
	"time"
)

// ContextEntry is one recorded fact: the key, its current value, and the step
// that last wrote it.
type ContextEntry struct {
	Key       string
	Value     string
	WrittenBy string
	WrittenAt string
}

// SetContext records key=value for a run, replacing any previous value.
//
// Last write wins rather than append-only: the store answers "what is true
// now", and a step that corrects an earlier fact must not leave the stale one
// readable beside it. The overwritten value is not kept — the transcript
// already records every call, and a second copy here would be a second source
// of truth about the same event.
func (s *Store) SetContext(ctx context.Context, runID, key, value, writtenBy string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_context (run_id, key, value, written_by, written_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (run_id, key) DO UPDATE SET
			value = excluded.value,
			written_by = excluded.written_by,
			written_at = excluded.written_at
	`, runID, key, value, writtenBy, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("could not record context key %q for run %q: %w", key, runID, err)
	}

	return nil
}

// RunContext returns every fact recorded for a run, ordered by key.
//
// Ordered by key rather than by write time so a rendered recap reads the same
// way twice — write order is whatever the model happened to do, which is not
// something a reader should have to hold in their head.
func (s *Store) RunContext(ctx context.Context, runID string) ([]ContextEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value, written_by, written_at FROM run_context WHERE run_id = ? ORDER BY key`, runID)
	if err != nil {
		return nil, fmt.Errorf("could not read context for run %q: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var entries []ContextEntry

	for rows.Next() {
		var entry ContextEntry

		err = rows.Scan(&entry.Key, &entry.Value, &entry.WrittenBy, &entry.WrittenAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan context for run %q: %w", runID, err)
		}

		entries = append(entries, entry)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read context for run %q: %w", runID, err)
	}

	return entries, nil
}
