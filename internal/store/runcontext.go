package store

// The run-scoped key/value store: what one agent step records with
// set_context, for the steps after it to read.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// NodeResult returns what a node recorded alongside its outcome, or nil when
// the node is unknown or recorded nothing.
//
// It exists for cache replay: a skipped step never runs, so whatever it
// recorded the first time has to come back from here or the run would disagree
// with an identical one that executed. Missing is not an error — most nodes
// record no result at all.
func (s *Store) NodeResult(ctx context.Context, hash string) (map[string]any, error) {
	var raw sql.NullString

	err := s.db.QueryRowContext(ctx, `SELECT result FROM nodes WHERE hash = ?`, hash).Scan(&raw)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // an unknown node is a legitimate "nothing recorded", not a failure
	}

	if err != nil {
		return nil, fmt.Errorf("could not read the result of node %q: %w", hash, err)
	}

	if !raw.Valid || raw.String == "" {
		return nil, nil //nolint:nilnil // a node that recorded no result is the common case
	}

	var result map[string]any

	err = json.Unmarshal([]byte(raw.String), &result)
	if err != nil {
		return nil, fmt.Errorf("could not decode the result of node %q: %w", hash, err)
	}

	return result, nil
}

// LayeredContext returns the facts visible from a nested write scope: every
// scope read in order, with a nearer one shadowing a farther one key by key,
// ordered by key like RunContext.
//
// It exists because writes and reads had different answers to "which scope".
// A concurrent branch writes into a scope only it touches, so its facts reach
// the run only at the join — which meant a step INSIDE the branch could not see
// what an earlier step of the same branch had recorded, while the identical two
// steps outside a block could. The asymmetry was invisible until someone nested
// an across: matrix in a branch.
//
// Nearest-wins rather than merged: a branch that corrects a fact established
// before the block should see its own correction, exactly as a later step
// outside a block sees the earlier step's overwrite.
func (s *Store) LayeredContext(ctx context.Context, scopes []string) ([]ContextEntry, error) {
	if len(scopes) == 1 {
		return s.RunContext(ctx, scopes[0])
	}

	visible := map[string]ContextEntry{}

	for _, scope := range scopes {
		entries, err := s.RunContext(ctx, scope)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			visible[entry.Key] = entry
		}
	}

	merged := make([]ContextEntry, 0, len(visible))
	for _, entry := range visible {
		merged = append(merged, entry)
	}

	// Sorted for RunContext's reason: a rendered recap has to read the same way
	// twice, and map order is not an order.
	sort.Slice(merged, func(i, j int) bool { return merged[i].Key < merged[j].Key })

	return merged, nil
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
