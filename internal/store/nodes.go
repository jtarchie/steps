package store

// nodes: the content-addressed record of what each step was and how it went,
// plus the transcripts agent nodes hang off.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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
//
// Two statements in one transaction because content is interned: the preimage
// goes into node_content under a hash of itself, and the node points at it. A
// single statement cannot do that, and doing it without the transaction would
// leave a window where the node's reference has no parent row — which the
// RESTRICT on that reference correctly refuses.
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	defer func() { _ = tx.Rollback() }()

	contentHash := contentKey(content)

	// DO NOTHING, not an update: the key IS the content, so a conflict means the
	// stored bytes already equal what would be written.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO node_content (content_hash, content) VALUES (?, ?)
		ON CONFLICT (content_hash) DO NOTHING
	`, contentHash, string(content))
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO nodes (hash, parent_hash, kind, job_name, resource, step_index, content_hash, result, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			parent_hash  = excluded.parent_hash,
			kind         = excluded.kind,
			job_name     = excluded.job_name,
			resource     = excluded.resource,
			step_index   = excluded.step_index,
			content_hash = excluded.content_hash,
			result       = excluded.result,
			status       = excluded.status,
			error        = excluded.error,
			created_at   = excluded.created_at
	`,
		node.Hash, nullableHash(node.ParentHash), node.Kind, jobName, node.Resource, node.StepIndex,
		contentHash, nullableString(resultJSON), status, errText(execErr), now(),
	)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return nil
}

// contentKey is the interning key: a hash OF the content bytes.
//
// Its own hash rather than the node's, because a node hash folds in its parent
// — two byte-identical steps in different chains must share one row here, and
// under a node hash they could not.
func contentKey(content []byte) string {
	sum := sha256.Sum256(content)

	return hex.EncodeToString(sum[:])
}

// nullableHash renders a chain-root's absent parent as NULL. The sentinel used
// to be an empty string, which a foreign key reads as a node whose hash is ""
// and demands exist, so it had to become the value sqlite exempts instead.
func nullableHash(hash string) any {
	if hash == "" {
		return nil
	}

	return hash
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
	// The join is what interning costs on the read side, and this is the only
	// read that pays it: content is display-only (the node-detail page), while
	// the list queries and every cache lookup never select it.
	rows, err := collect(ctx, s.db, "nodes by hash", `
		SELECT n.hash, n.kind, n.job_name, n.resource, n.step_index, n.status, n.error, n.result,
		       n.created_at, c.content, COALESCE(n.parent_hash, '')
		FROM nodes n
		JOIN node_content c ON c.content_hash = n.content_hash
		WHERE n.hash IN (`+placeholders(len(hashes))+`)`, args, func(rows *sql.Rows) (NodeRow, error) {
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

// MaxTranscriptBytes bounds one stored transcript.
//
// internal/agent already caps a single tool RESULT (maxRecordedResultBytes),
// which bounds the widest single value in a transcript but not the transcript:
// a conversation has as many turns as it needs, and an agent that worked for an
// hour writes all of them into one row. The largest value in the schema was
// therefore the one with no total bound at all.
//
// 256KB is far past any transcript worth reading end to end while staying an
// order of magnitude below the point where one row dominates the database. A
// transcript is a diagnostic, not a ledger: nothing reads it to make a
// decision, so losing the tail of a very long one costs nothing that the
// per-step token counts in agent_usage do not still record exactly.
const MaxTranscriptBytes = 256 * 1024

// truncationEvent is appended in place of the entries dropped at the cap, so a
// reader who reaches the end knows the conversation continued rather than
// stopped. Spelled as a transcript event because that is what the consumers
// decode (internal/agent's transcriptEvent, rendered by internal/web).
const truncationEvent = `{"type":"text","text":"[transcript truncated: over the stored size limit]"}`

// truncateTranscript cuts a transcript to at most MaxTranscriptBytes while
// keeping it VALID JSON, by dropping whole events off the end.
//
// The first version sliced the string at a byte offset and appended a plain-text
// notice. That produced `[{"type":"text","text":"ttt` + a bare newline — invalid
// JSON, so json.Unmarshal in the renderer failed and every over-cap transcript
// displayed as NO transcript at all, the exact opposite of keeping the head. It
// passed a test asserting only that the stored string was non-empty. Byte
// slicing could also cut a multi-byte rune in half.
//
// Events are dropped from the TAIL: the task, the first tool calls and the early
// decisions are what explain what a step was doing, and a conversation long
// enough to hit the cap is one whose middle is mostly repetition.
//
// Anything that does not parse as a JSON array is passed through untouched under
// a hard byte cap. This function's job is a storage bound, not validation — a
// caller that hands over something else has a bug of its own, and silently
// rewriting it would hide that.
func truncateTranscript(transcript string) string {
	if len(transcript) <= MaxTranscriptBytes {
		return transcript
	}

	var events []json.RawMessage

	err := json.Unmarshal([]byte(transcript), &events)
	if err != nil || len(events) == 0 {
		return truncateUTF8(transcript, MaxTranscriptBytes)
	}

	// Two brackets, the notice, and the comma before it.
	budget := MaxTranscriptBytes - len(truncationEvent) - len(`[,]`)

	kept, used := 0, 0

	for _, event := range events {
		// Each event costs its own bytes plus the comma joining it to the last.
		cost := len(event)
		if kept > 0 {
			cost++
		}

		if used+cost > budget {
			break
		}

		used += cost
		kept++
	}

	parts := make([]string, 0, kept+1)
	for _, event := range events[:kept] {
		parts = append(parts, string(event))
	}

	parts = append(parts, truncationEvent)

	return "[" + strings.Join(parts, ",") + "]"
}

// truncateUTF8 cuts s to at most a byte limit without splitting a rune.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	cut := limit
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}

	return s[:cut]
}

// SaveNodeTranscript stores (or replaces) an agent node's full conversation
// transcript, a JSON array of events. Kept in its own table so nodes.result —
// which rides along on every node listing — stays bounded; a transcript is
// read on demand instead.
//
// Truncated at MaxTranscriptBytes, keeping the HEAD: the task, the first tool
// calls and the early decisions are what explain what a step was doing, and a
// conversation that ran long enough to hit the cap is one whose middle is
// mostly repetition.
func (s *Store) SaveNodeTranscript(ctx context.Context, hash, transcript string) error {
	transcript = truncateTranscript(transcript)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_transcripts (hash, transcript)
		VALUES (?, ?)
		ON CONFLICT (hash) DO UPDATE SET
			transcript = excluded.transcript
	`, hash, transcript)
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
