package store

// Reading back what a run recorded.
//
// Everything here was already being written — statuses, error text, agent
// results, the trigger queue — and none of it was readable except by opening
// the sqlite file by hand and knowing the schema. Since the driver is pure Go
// and vendored, the `sqlite3` binary needed to do that isn't necessarily even
// installed. These are the queries `steps runs` is built on.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// JobRunRow is one recorded job run.
type JobRunRow struct {
	JobName   string
	RootHash  string
	Status    string
	Error     string
	CreatedAt time.Time
}

// NodeRow is one recorded step, with whatever the step produced.
type NodeRow struct {
	Hash      string
	Kind      string
	JobName   string
	Resource  string
	StepIndex int
	Status    string
	Error     string
	Result    string
	CreatedAt time.Time
}

// QueueRow is one entry in the downstream-trigger queue.
type QueueRow struct {
	ID         int64
	JobName    string
	Reason     string
	Status     string
	EnqueuedAt time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}

// ListJobRuns returns the most recent job runs, newest first. An empty
// jobName covers every job.
func (s *Store) ListJobRuns(ctx context.Context, jobName string, limit int) ([]JobRunRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT job_name, root_hash, status, error, created_at
		FROM job_runs
		WHERE (? = '' OR job_name = ?)
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?
	`, jobName, jobName, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query job_runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []JobRunRow

	for rows.Next() {
		var (
			row       JobRunRow
			errText   sql.NullString
			createdAt string
		)

		err = rows.Scan(&row.JobName, &row.RootHash, &row.Status, &errText, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan job_runs row: %w", err)
		}

		row.Error = errText.String
		row.CreatedAt = parseTimestamp(createdAt)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read job_runs: %w", err)
	}

	return out, nil
}

// ListNodes returns the most recently recorded steps, newest first. An empty
// jobName covers every job.
func (s *Store) ListNodes(ctx context.Context, jobName string, limit int) ([]NodeRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT hash, kind, job_name, resource, step_index, status, error, result, created_at
		FROM nodes
		WHERE (? = '' OR job_name = ?)
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?
	`, jobName, jobName, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NodeRow

	for rows.Next() {
		var (
			row               NodeRow
			errText, result   sql.NullString
			createdAtRaw      string
			resource, kindRaw string
		)

		err = rows.Scan(&row.Hash, &kindRaw, &row.JobName, &resource, &row.StepIndex,
			&row.Status, &errText, &result, &createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("could not scan nodes row: %w", err)
		}

		row.Kind, row.Resource = kindRaw, resource
		row.Error, row.Result = errText.String, result.String
		row.CreatedAt = parseTimestamp(createdAtRaw)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read nodes: %w", err)
	}

	return out, nil
}

// ListTriggerQueue returns the most recent trigger-queue entries, newest
// first — what `steps watch` has queued, run, or failed to run.
func (s *Store) ListTriggerQueue(ctx context.Context, limit int) ([]QueueRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, reason, status, enqueued_at, started_at, finished_at, error
		FROM trigger_queue
		ORDER BY id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query trigger_queue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []QueueRow

	for rows.Next() {
		var (
			row                             QueueRow
			started, finished, errText      sql.NullString
			enqueuedAt                      string
			startedAt, finishedAt, errValue string
		)

		err = rows.Scan(&row.ID, &row.JobName, &row.Reason, &row.Status,
			&enqueuedAt, &started, &finished, &errText)
		if err != nil {
			return nil, fmt.Errorf("could not scan trigger_queue row: %w", err)
		}

		startedAt, finishedAt, errValue = started.String, finished.String, errText.String

		row.EnqueuedAt = parseTimestamp(enqueuedAt)
		row.StartedAt = parseTimestamp(startedAt)
		row.FinishedAt = parseTimestamp(finishedAt)
		row.Error = errValue
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read trigger_queue: %w", err)
	}

	return out, nil
}

// parseTimestamp turns a stored RFC3339 string into a Time, yielding the zero
// Time for an empty or unparseable value. Reading history is a diagnostic, so
// a malformed row is better rendered blank than fatal.
func parseTimestamp(value string) time.Time {
	if value == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}

	return parsed
}
