package store

// nodes: the content-addressed record of what each step was and how it went,
// plus the transcripts agent nodes hang off.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// NodeRecord is the subset of a merkle plan node's fields this package
// persists. It's a plain data shape rather than an import of merkle.Node so
// this leaf package doesn't need to depend on the planner — callers convert
// their own Node type into one of these.
type NodeRecord struct {
	Hash       string
	ParentHash string
	Kind       string
	StepIndex  int
	Resource   string // resource name (get/put) or task name (task); metadata only
	Content    map[string]any
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

// RecordNode upserts a node's execution outcome, keyed by its content hash.
func (s *Store) RecordNode(ctx context.Context, node NodeRecord, jobName, status string, result map[string]any, execErr error) error {
	content, err := json.Marshal(node.Content)
	if err != nil {
		return fmt.Errorf("could not marshal node content: %w", err)
	}

	var resultJSON []byte

	if result != nil {
		resultJSON, err = json.Marshal(result)
		if err != nil {
			return fmt.Errorf("could not marshal node result: %w", err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO nodes (hash, parent_hash, kind, job_name, resource, step_index, content, result, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			parent_hash = excluded.parent_hash,
			kind        = excluded.kind,
			job_name    = excluded.job_name,
			resource    = excluded.resource,
			step_index  = excluded.step_index,
			content     = excluded.content,
			result      = excluded.result,
			status      = excluded.status,
			error       = excluded.error,
			created_at  = excluded.created_at
	`,
		node.Hash, node.ParentHash, node.Kind, jobName, node.Resource, node.StepIndex,
		string(content), nullableString(resultJSON), status, errText(execErr), now(),
	)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return nil
}

// HasNodeSucceeded reports whether a node with this exact hash has already
// been recorded as succeeded for this job.
//
// It is per-NODE memoization, distinct from HasSucceeded's per-CHAIN check.
// The chain form asks "did this whole path succeed", which is right for a
// sequence: a changed step invalidates everything after it. An across: cell
// has no such sequence — cells are siblings, and one cell changing says
// nothing about another — so a cell asks about itself alone.
func (s *Store) HasNodeSucceeded(ctx context.Context, jobName, hash string) (bool, error) {
	var count int

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE hash = ? AND job_name = ? AND status = 'succeeded'`,
		hash, jobName).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("could not read node %q: %w", hash, err)
	}

	return count > 0, nil
}

// ListNodes returns the most recently recorded steps, newest first. An empty
// jobName covers every job.
func (s *Store) ListNodes(ctx context.Context, jobName string, limit int) ([]NodeRow, error) {
	return collect(ctx, s.db, "nodes", `
		SELECT hash, kind, job_name, resource, step_index, status, error, result, created_at
		FROM nodes
		WHERE (? = '' OR job_name = ?)
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?
	`, []any{jobName, jobName, limit}, func(rows *sql.Rows) (NodeRow, error) {
		var (
			row            NodeRow
			errCol, result sql.NullString
			createdAt      string
		)

		err := rows.Scan(&row.Hash, &row.Kind, &row.JobName, &row.Resource, &row.StepIndex,
			&row.Status, &errCol, &result, &createdAt)

		row.Error, row.Result = errCol.String, result.String
		row.CreatedAt = parseTimestamp(createdAt)

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// NodesByHash returns the recorded nodes for the given hashes, keyed by hash
// — one query for a whole run transcript rather than one per step.
func (s *Store) NodesByHash(ctx context.Context, hashes []string) (map[string]NodeRow, error) {
	found := map[string]NodeRow{}
	if len(hashes) == 0 {
		return found, nil
	}

	args := make([]any, 0, len(hashes))
	for _, hash := range hashes {
		args = append(args, hash)
	}

	// The only thing concatenated is the placeholder list itself — a run of
	// "?," generated from len(hashes). Every hash travels as a bound argument,
	// so no caller-supplied text reaches the SQL. sqlite has no array-binding
	// form, which is why the placeholder count must be built rather than
	// parameterized.
	rows, err := collect(ctx, s.db, "nodes by hash", `
		SELECT hash, kind, job_name, resource, step_index, status, error, result, created_at, content, parent_hash
		FROM nodes WHERE hash IN (`+placeholders(len(hashes))+`)`, args, func(rows *sql.Rows) (NodeRow, error) {
		var (
			row            NodeRow
			errCol, result sql.NullString
			createdAt      string
		)

		err := rows.Scan(&row.Hash, &row.Kind, &row.JobName, &row.Resource, &row.StepIndex,
			&row.Status, &errCol, &result, &createdAt, &row.Content, &row.ParentHash)

		row.Error, row.Result = errCol.String, result.String
		row.CreatedAt = parseTimestamp(createdAt)

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		found[row.Hash] = row
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

// placeholders builds the "?,?,?" list for an IN (...) clause of n bound
// arguments. sqlite has no array binding, so the count has to be generated.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// SaveNodeTranscript stores (or replaces) an agent node's full conversation
// transcript, a JSON array of events. Kept in its own table so nodes.result —
// which planners and routed-to successors load on every run — stays bounded;
// a transcript is read on demand instead.
func (s *Store) SaveNodeTranscript(ctx context.Context, hash, jobName, transcript string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_transcripts (hash, job_name, transcript, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (hash) DO UPDATE SET
			job_name = excluded.job_name,
			transcript = excluded.transcript,
			created_at = excluded.created_at
	`, hash, jobName, transcript, now())
	if err != nil {
		return fmt.Errorf("could not save transcript for node %q: %w", hash, err)
	}

	return nil
}

// NodeTranscript returns the stored transcript JSON for a node hash, with ok
// reporting whether one exists — mirroring LastCheckedVersion's shape rather
// than inventing a sentinel error for the common "never recorded" case.
func (s *Store) NodeTranscript(ctx context.Context, hash string) (string, bool, error) {
	var transcript string

	err := s.db.QueryRowContext(ctx,
		`SELECT transcript FROM node_transcripts WHERE hash = ?`, hash).Scan(&transcript)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("could not read transcript for node %q: %w", hash, err)
	}

	return transcript, true, nil
}
