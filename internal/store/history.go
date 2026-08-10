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
	"errors"
	"fmt"
	"strings"
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
	// Content and ParentHash are what the hash is MADE of — populated by
	// NodesByHash (the node-detail read), left empty by the list queries,
	// whose callers want a table row rather than a whole content map.
	Content    string
	ParentHash string
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

// RunRow is one run invocation as the history views read it: the resume
// record (runs) plus the finish timestamp that makes a duration answerable.
type RunRow struct {
	ID         string
	JobName    string
	Workspace  string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
}

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

// ListRuns returns run invocations, newest first. An empty jobName covers
// every job.
//
// This reads `runs`, not `job_runs`: job_runs records only chains that are
// reusable (or that failed), so a successful run containing a put or an agent
// records nothing there at all — it is a cache index, never a history. runs
// has exactly one row per invocation, which is what a history is.
func (s *Store) ListRuns(ctx context.Context, jobName string, limit int) ([]RunRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, workspace, status, started_at, COALESCE(finished_at, '')
		FROM runs
		WHERE (? = '' OR job_name = ?)
		ORDER BY started_at DESC, rowid DESC
		LIMIT ?
	`, jobName, jobName, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunRow

	for rows.Next() {
		var (
			row                   RunRow
			startedAt, finishedAt string
		)

		err = rows.Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan runs row: %w", err)
		}

		row.StartedAt = parseTimestamp(startedAt)
		row.FinishedAt = parseTimestamp(finishedAt)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read runs: %w", err)
	}

	return out, nil
}

// LatestRunByJob returns the most recent run for every job that has one,
// keyed by job name — one query for a jobs board rather than one per job.
func (s *Store) LatestRunByJob(ctx context.Context) (map[string]RunRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.job_name, r.workspace, r.status, r.started_at, COALESCE(r.finished_at, '')
		FROM runs r
		JOIN (SELECT job_name, MAX(started_at) AS latest FROM runs GROUP BY job_name) m
		  ON m.job_name = r.job_name AND m.latest = r.started_at
	`)
	if err != nil {
		return nil, fmt.Errorf("could not query latest runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	latest := map[string]RunRow{}

	for rows.Next() {
		var (
			row                   RunRow
			startedAt, finishedAt string
		)

		err = rows.Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan latest run row: %w", err)
		}

		row.StartedAt = parseTimestamp(startedAt)
		row.FinishedAt = parseTimestamp(finishedAt)

		// Two runs of one job can share a started_at second, so the join can
		// yield both; keep whichever sorts later by id for a stable answer.
		if prior, ok := latest[row.JobName]; ok && prior.ID > row.ID {
			continue
		}

		latest[row.JobName] = row
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read latest runs: %w", err)
	}

	return latest, nil
}

// RunEventRow is one persisted run event — the stored form of
// events.Event, which is what a finished run is replayed from.
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

// AppendRunEvent persists one run event. Called from the bus's sink
// goroutine (internal/events), so writes are already serialized.
func (s *Store) AppendRunEvent(ctx context.Context, row RunEventRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_events
			(run_id, job_name, type, step_index, step_name, step_kind, status, hash, text, name, detail, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, row.RunID, row.JobName, row.Type, row.StepIndex, row.StepName, row.StepKind,
		row.Status, row.Hash, row.Text, row.Name, row.Detail, row.DurationMS,
		row.At.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("could not append run event for %q: %w", row.RunID, err)
	}

	return nil
}

// RunEvents replays a run's events in order, from afterSeq exclusive. Pass 0
// for the whole run — which is also how a reconnecting live view catches up
// on what it missed without re-reading what it already has.
func (s *Store) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]RunEventRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, run_id, job_name, type, step_index, step_name, step_kind,
		       status, hash, text, name, detail, duration_ms, created_at
		FROM run_events
		WHERE run_id = ? AND seq > ?
		ORDER BY seq
		LIMIT ?
	`, runID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query run events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunEventRow

	for rows.Next() {
		var (
			row       RunEventRow
			createdAt string
		)

		err = rows.Scan(&row.Seq, &row.RunID, &row.JobName, &row.Type, &row.StepIndex,
			&row.StepName, &row.StepKind, &row.Status, &row.Hash, &row.Text,
			&row.Name, &row.Detail, &row.DurationMS, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan run event: %w", err)
		}

		row.At = parseTimestamp(createdAt)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read run events: %w", err)
	}

	return out, nil
}

// NodesByHash returns the recorded nodes for the given hashes, keyed by hash
// — one query for a whole run transcript rather than one per step.
func (s *Store) NodesByHash(ctx context.Context, hashes []string) (map[string]NodeRow, error) {
	found := map[string]NodeRow{}
	if len(hashes) == 0 {
		return found, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
	args := make([]any, 0, len(hashes))

	for _, hash := range hashes {
		args = append(args, hash)
	}

	// The only thing concatenated is the placeholder list itself — a run of
	// "?," generated from len(hashes) above. Every hash travels as a bound
	// argument, so no caller-supplied text reaches the SQL text. sqlite has no
	// array-binding form, which is why the placeholder count must be built
	// rather than parameterized.
	//nolint:gosec // G202: the concatenated fragment is generated placeholders, never input
	rows, err := s.db.QueryContext(ctx, `
		SELECT hash, kind, job_name, resource, step_index, status, error, result, created_at, content, parent_hash
		FROM nodes WHERE hash IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("could not query nodes by hash: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			row             NodeRow
			errText, result sql.NullString
			createdAt       string
		)

		err = rows.Scan(&row.Hash, &row.Kind, &row.JobName, &row.Resource, &row.StepIndex,
			&row.Status, &errText, &result, &createdAt, &row.Content, &row.ParentHash)
		if err != nil {
			return nil, fmt.Errorf("could not scan node: %w", err)
		}

		row.Error, row.Result = errText.String, result.String
		row.CreatedAt = parseTimestamp(createdAt)
		found[row.Hash] = row
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read nodes by hash: %w", err)
	}

	return found, nil
}

// FindNode reads one node by hash, with ok reporting whether it exists.
func (s *Store) FindNode(ctx context.Context, hash string) (NodeRow, bool, error) {
	byHash, err := s.NodesByHash(ctx, []string{hash})
	if err != nil {
		return NodeRow{}, false, err
	}

	row, ok := byHash[hash]

	return row, ok, nil
}

// RunsUsingNode lists the runs whose events reference a node hash — the
// "which runs reused this cached step" answer a node page is built on.
func (s *Store) RunsUsingNode(ctx context.Context, hash string, limit int) ([]RunRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.job_name, r.workspace, r.status, r.started_at, COALESCE(r.finished_at, '')
		FROM runs r
		WHERE r.id IN (SELECT DISTINCT run_id FROM run_events WHERE hash = ?)
		ORDER BY r.started_at DESC
		LIMIT ?
	`, hash, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query runs using node: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RunRow

	for rows.Next() {
		var (
			row                   RunRow
			startedAt, finishedAt string
		)

		err = rows.Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan run using node: %w", err)
		}

		row.StartedAt = parseTimestamp(startedAt)
		row.FinishedAt = parseTimestamp(finishedAt)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read runs using node: %w", err)
	}

	return out, nil
}

// PassedVersions lists the resource versions a job has recorded as green —
// what a downstream passed: constraint is satisfied by.
func (s *Store) PassedVersions(ctx context.Context, jobName string, limit int) ([]PassedVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT resource_name, version_json, recorded_at
		FROM job_versions WHERE job_name = ?
		ORDER BY recorded_at DESC LIMIT ?
	`, jobName, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query job versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PassedVersion

	for rows.Next() {
		var (
			row        PassedVersion
			recordedAt string
		)

		err = rows.Scan(&row.Resource, &row.Version, &recordedAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan job version: %w", err)
		}

		row.RecordedAt = parseTimestamp(recordedAt)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read job versions: %w", err)
	}

	return out, nil
}

// PassedVersion is one resource version a job succeeded against.
type PassedVersion struct {
	Resource   string
	Version    string
	RecordedAt time.Time
}

// CheckedResource is a resource's last observed version.
type CheckedResource struct {
	Name      string
	Version   string
	CheckedAt time.Time
}

// CheckedResources lists every resource version the watcher has recorded.
func (s *Store) CheckedResources(ctx context.Context) ([]CheckedResource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT resource_name, version_json, checked_at FROM resource_checks ORDER BY resource_name
	`)
	if err != nil {
		return nil, fmt.Errorf("could not query resource checks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CheckedResource

	for rows.Next() {
		var (
			row       CheckedResource
			checkedAt string
		)

		err = rows.Scan(&row.Name, &row.Version, &checkedAt)
		if err != nil {
			return nil, fmt.Errorf("could not scan resource check: %w", err)
		}

		row.CheckedAt = parseTimestamp(checkedAt)
		out = append(out, row)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read resource checks: %w", err)
	}

	return out, nil
}

// AllApprovals lists every approval decision, newest first — the audit trail
// PendingApprovals deliberately does not carry.
func (s *Store) AllApprovals(ctx context.Context, limit int) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, '')
		FROM approvals ORDER BY id DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("could not query approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Approval

	for rows.Next() {
		var approval Approval

		err = rows.Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.Status,
			&approval.RequestedAt, &approval.DecidedAt, &approval.DecidedBy, &approval.Reason)
		if err != nil {
			return nil, fmt.Errorf("could not scan approval: %w", err)
		}

		out = append(out, approval)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read approvals: %w", err)
	}

	return out, nil
}

// FindRunRow reads one run in the history shape, with its finish timestamp —
// so a single run's page and the run list render from identical data. (Store.
// FindRun returns the resume shape, which predates finished_at and is used by
// --resume, where the finish time is irrelevant.)
func (s *Store) FindRunRow(ctx context.Context, id string) (RunRow, bool, error) {
	var (
		row                   RunRow
		startedAt, finishedAt string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, job_name, workspace, status, started_at, COALESCE(finished_at, '')
		FROM runs WHERE id = ?
	`, id).Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRow{}, false, nil
	}

	if err != nil {
		return RunRow{}, false, fmt.Errorf("could not read run %q: %w", id, err)
	}

	row.StartedAt = parseTimestamp(startedAt)
	row.FinishedAt = parseTimestamp(finishedAt)

	return row, true, nil
}

// FirstRunSince returns the oldest run of a job started at or after `since`,
// with ok reporting whether one exists yet.
//
// It backs the trigger-and-follow handoff: a queued job has no run id until
// the worker claims it and RunJob mints one, so the browser asks this until
// the run it caused appears. Oldest-first rather than newest, so a burst of
// triggers hands each caller the run its own click produced.
func (s *Store) FirstRunSince(ctx context.Context, jobName string, since time.Time) (RunRow, bool, error) {
	var (
		row                   RunRow
		startedAt, finishedAt string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, job_name, workspace, status, started_at, COALESCE(finished_at, '')
		FROM runs
		WHERE job_name = ? AND started_at >= ?
		ORDER BY started_at, rowid
		LIMIT 1
	`, jobName, since.UTC().Format(time.RFC3339Nano)).
		Scan(&row.ID, &row.JobName, &row.Workspace, &row.Status, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRow{}, false, nil
	}

	if err != nil {
		return RunRow{}, false, fmt.Errorf("could not look for a run of %q: %w", jobName, err)
	}

	row.StartedAt = parseTimestamp(startedAt)
	row.FinishedAt = parseTimestamp(finishedAt)

	return row, true, nil
}
