package postgres

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

	"github.com/jtarchie/steps/internal/store"
)

// RecordNode upserts a node's execution outcome, interning its content.
//
// It holds the content lock SHARED, and the node_content sweep takes it
// exclusive. node_content is the one table every pipeline in the schema
// shares, and ON CONFLICT DO NOTHING finding a row present takes no lock on
// it — so without this, another pipeline's sweep could delete the row between
// this statement seeing it and the node's reference to it, failing the
// foreign key. sqlite serialized the file, so the window never existed there.
// Shared, so recording nodes never waits on another recording.
func (s *Store) RecordNode(ctx context.Context, node store.NodeRecord, jobName, status string, result map[string]any, execErr error) error {
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

	contentHash := contentKey(content)

	err = s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared($1, 0)`, classContent)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the node
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO node_content (content_hash, content) VALUES ($1, $2)
			ON CONFLICT (content_hash) DO NOTHING
		`, contentHash, clean(string(content)))
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the node
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO nodes (pipeline_id, hash, parent_hash, kind, job_name, resource, step_index, content_hash, result, status, error, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (pipeline_id, hash) DO UPDATE SET
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
			s.pipelineID, node.Hash, nullable(node.ParentHash), node.Kind, jobName, node.Resource, node.StepIndex,
			contentHash, nullableBytes(resultJSON), status, errText(execErr), now(),
		)

		return err //nolint:wrapcheck // wrapped below with the node
	})
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return nil
}

// contentKey is the interning key: a hash OF the content bytes.
func contentKey(content []byte) string {
	sum := sha256.Sum256(content)

	return hex.EncodeToString(sum[:])
}

// HasNodeSucceeded reports whether this exact node succeeded for this job.
func (s *Store) HasNodeSucceeded(ctx context.Context, jobName, hash string) (bool, error) {
	var found bool

	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM nodes WHERE pipeline_id = $1 AND hash = $2 AND job_name = $3 AND status = 'succeeded')`,
		s.pipelineID, hash, jobName).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("could not read node %q: %w", hash, err)
	}

	return found, nil
}

// ListNodes returns the most recently recorded steps, newest first. An empty
// jobName covers every job.
func (s *Store) ListNodes(ctx context.Context, jobName string, limit int) ([]store.NodeRow, error) {
	filter, args := byJob(jobName, []any{s.pipelineID})

	return collect(ctx, s.db, "nodes", `
		SELECT hash, kind, job_name, resource, step_index, status, error, result, created_at
		FROM nodes
		WHERE pipeline_id = $1`+filter+`
		ORDER BY created_at DESC, seq DESC
		LIMIT `+next(args), append(args, rowLimit(limit)), func(rows *sql.Rows) (store.NodeRow, error) {
		var (
			row            store.NodeRow
			errCol, result sql.NullString
			createdAt      time.Time
		)

		err := rows.Scan(&row.Hash, &row.Kind, &row.JobName, &row.Resource, &row.StepIndex,
			&row.Status, &errCol, &result, &createdAt)

		row.Error, row.Result = errCol.String, result.String
		row.CreatedAt = createdAt.UTC()

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// NodesByHash returns the recorded nodes for the given hashes, keyed by hash.
func (s *Store) NodesByHash(ctx context.Context, hashes []string) (map[string]store.NodeRow, error) {
	found := map[string]store.NodeRow{}
	if len(hashes) == 0 {
		return found, nil
	}

	rows, err := collect(ctx, s.db, "nodes by hash", `
		SELECT n.hash, n.kind, n.job_name, n.resource, n.step_index, n.status, n.error, n.result,
		       n.created_at, c.content, COALESCE(n.parent_hash, '')
		FROM nodes n
		JOIN node_content c ON c.content_hash = n.content_hash
		WHERE n.pipeline_id = $1 AND n.hash = ANY($2::text[])`,
		[]any{s.pipelineID, hashes}, func(rows *sql.Rows) (store.NodeRow, error) {
			var (
				row            store.NodeRow
				errCol, result sql.NullString
				createdAt      time.Time
			)

			err := rows.Scan(&row.Hash, &row.Kind, &row.JobName, &row.Resource, &row.StepIndex,
				&row.Status, &errCol, &result, &createdAt, &row.Content, &row.ParentHash)

			row.Error, row.Result = errCol.String, result.String
			row.CreatedAt = createdAt.UTC()

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

// truncationEvent replaces the events dropped at the cap; see the sqlite
// driver's truncateTranscript.
const truncationEvent = `{"type":"text","text":"[transcript truncated: over the stored size limit]"}`

// truncateTranscript cuts a transcript to MaxTranscriptBytes while keeping it
// valid JSON, dropping whole events off the tail.
func truncateTranscript(transcript string) string {
	if len(transcript) <= store.MaxTranscriptBytes {
		return transcript
	}

	var events []json.RawMessage

	err := json.Unmarshal([]byte(transcript), &events)
	if err != nil || len(events) == 0 {
		return truncateUTF8(transcript, store.MaxTranscriptBytes)
	}

	budget := store.MaxTranscriptBytes - len(truncationEvent) - len(`[,]`)

	kept, used := 0, 0

	for _, event := range events {
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

// SaveNodeTranscript stores (or replaces) an agent node's transcript, capped
// at MaxTranscriptBytes.
func (s *Store) SaveNodeTranscript(ctx context.Context, hash, transcript string) error {
	transcript = truncateTranscript(clean(transcript))

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_transcripts (pipeline_id, hash, transcript)
		VALUES ($1, $2, $3)
		ON CONFLICT (pipeline_id, hash) DO UPDATE SET
			transcript = excluded.transcript
	`, s.pipelineID, hash, transcript)
	if err != nil {
		return fmt.Errorf("could not save transcript for node %q: %w", hash, err)
	}

	return nil
}

// NodeTranscript returns the stored transcript for a node hash.
func (s *Store) NodeTranscript(ctx context.Context, hash string) (string, bool, error) {
	var transcript string

	err := s.db.QueryRowContext(ctx,
		`SELECT transcript FROM node_transcripts WHERE pipeline_id = $1 AND hash = $2`,
		s.pipelineID, hash).Scan(&transcript)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("could not read transcript for node %q: %w", hash, err)
	}

	return transcript, true, nil
}

// RecordChainSucceeded adds (jobName, rootHash) to the skip index.
func (s *Store) RecordChainSucceeded(ctx context.Context, jobName, rootHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_runs (pipeline_id, job_name, root_hash) VALUES ($1, $2, $3)
		ON CONFLICT (pipeline_id, job_name, root_hash) DO NOTHING
	`, s.pipelineID, jobName, rootHash)
	if err != nil {
		return fmt.Errorf("could not record job run (job %q, root %q): %w", jobName, rootHash, err)
	}

	return nil
}

// ForgetChain removes (jobName, rootHash) from the skip index.
func (s *Store) ForgetChain(ctx context.Context, jobName, rootHash string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM job_runs WHERE pipeline_id = $1 AND job_name = $2 AND root_hash = $3`,
		s.pipelineID, jobName, rootHash)
	if err != nil {
		return fmt.Errorf("could not forget job run (job %q, root %q): %w", jobName, rootHash, err)
	}

	return nil
}

// HasSucceededBatch reports which of rootHashes have succeeded for jobName,
// in one round trip.
func (s *Store) HasSucceededBatch(ctx context.Context, jobName string, rootHashes []string) (map[string]bool, error) {
	found, err := collect(ctx, s.db, "job_runs",
		`SELECT root_hash FROM job_runs
		 WHERE pipeline_id = $1 AND job_name = $2 AND root_hash = ANY($3::text[])`,
		[]any{s.pipelineID, jobName, textArray(rootHashes)}, scanString)
	if err != nil {
		return nil, err
	}

	result := make(map[string]bool, len(found))
	for _, hash := range found {
		result[hash] = true
	}

	return result, nil
}
