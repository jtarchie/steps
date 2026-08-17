package store

// run_events: the persisted side of the run-event bus (internal/events), so a
// finished run reads back exactly as it read live.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RunEventRow is one persisted run event — the stored form of events.Event,
// which is what a finished run is replayed from.
type RunEventRow struct {
	Seq        int64
	RunID      string
	JobName    string
	Type       string
	StepIndex  int
	StepName   string
	StepKind   string
	Status     string
	Hash       string
	Text       string
	Name       string
	Detail     string
	DurationMS int64
	At         time.Time
}

// MaxEventTextBytes bounds the free-text columns of one run event.
//
// The publishers already bound what they MEAN to store — a step's output at
// 32,000 bytes (pipeline's maxPublishedOutputBytes), a tool result at 16,384
// (agent's maxRecordedResultBytes) — but nothing bounded an event carrying an
// ERROR, and one error is routinely enormous: a failing check or task reports
// `command %q failed`, where the command is the whole generated shell script,
// about 1.3KB for the built-in git check. errText caps that for nodes.error and
// trigger_queue.error; this event went in verbatim, into the table with the most
// rows, on every failing step of every poll.
//
// Set above every deliberate publisher cap so it never truncates something a
// publisher chose to keep — it is the backstop for the columns nobody capped,
// not a second opinion on the ones they did.
const MaxEventTextBytes = 64 * 1024

// AppendRunEvent persists one run event. Called from the bus's sink
// goroutine (internal/events), so writes are already serialized.
func (s *Store) AppendRunEvent(ctx context.Context, row RunEventRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_events
			(run_id, job_name, type, step_index, step_name, step_kind, status, hash, text, name, detail, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, row.RunID, row.JobName, row.Type, row.StepIndex, row.StepName, row.StepKind,
		row.Status, row.Hash,
		truncateUTF8(row.Text, MaxEventTextBytes),
		truncateUTF8(row.Name, MaxEventTextBytes),
		truncateUTF8(row.Detail, MaxEventTextBytes),
		row.DurationMS,
		row.At.UTC().Format(sortableNano))
	if err != nil {
		return fmt.Errorf("could not append run event for %q: %w", row.RunID, err)
	}

	return nil
}

// RunEvents replays a run's events in order, from afterSeq exclusive. Pass 0
// for the whole run — which is also how a reconnecting live view catches up
// on what it missed without re-reading what it already has.
func (s *Store) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]RunEventRow, error) {
	return collect(ctx, s.db, "run events", `
		SELECT seq, run_id, job_name, type, step_index, step_name, step_kind,
		       status, hash, text, name, detail, duration_ms, created_at
		FROM run_events
		WHERE run_id = ? AND seq > ?
		ORDER BY seq
		LIMIT ?
	`, []any{runID, afterSeq, limit}, func(rows *sql.Rows) (RunEventRow, error) {
		var (
			row       RunEventRow
			createdAt string
		)

		err := rows.Scan(&row.Seq, &row.RunID, &row.JobName, &row.Type, &row.StepIndex,
			&row.StepName, &row.StepKind, &row.Status, &row.Hash, &row.Text,
			&row.Name, &row.Detail, &row.DurationMS, &createdAt)

		row.At = parseTimestamp(createdAt)

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}
