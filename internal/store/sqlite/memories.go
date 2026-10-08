package sqlite

// memories: what an agent step keeps about a scope from one run to the next.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// Remember files the memory, or finds it already filed, and applies the cap,
// in one transaction.
//
// The run id goes through a scoped subquery rather than straight in: a run of
// a SIBLING pipeline satisfies the foreign key, and a memory should not claim
// a run this pipeline cannot show. A run that is not this pipeline's files
// NULL, the same as one retention has reaped.
func (s *Store) Remember(ctx context.Context, memory store.Memory, limit int) (store.Memory, bool, error) {
	err := store.CheckMemory(memory)
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO memories (pipeline_id, scope, text, run_id, created_at)
		VALUES (?, ?, ?, (SELECT id FROM runs WHERE id = ? AND pipeline_id = ?), ?)
		ON CONFLICT (pipeline_id, scope, text) DO NOTHING
	`, s.pipelineID, memory.Scope, memory.Text, memory.RunID, s.pipelineID, nowNano())
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	err = evictMemories(ctx, tx, s.pipelineID, memory.Scope, limit)
	if err != nil {
		return store.Memory{}, false, err
	}

	stored, err := scanMemory(tx.QueryRowContext(ctx, memoryColumns+`
		WHERE pipeline_id = ? AND scope = ? AND text = ?
	`, s.pipelineID, memory.Scope, memory.Text))
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	err = tx.Commit()
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	return stored, affected > 0, nil
}

// evictMemories keeps a scope's newest limit entries. By id rather than
// created_at: AUTOINCREMENT is insertion order with no ties.
func evictMemories(ctx context.Context, tx *sql.Tx, pipelineID int64, scope string, limit int) error {
	if limit < 0 {
		limit = store.DefaultMemoryEntries
	}

	if limit == 0 {
		return nil
	}

	_, err := tx.ExecContext(ctx, `
		DELETE FROM memories
		WHERE pipeline_id = ? AND scope = ? AND id NOT IN (
			SELECT id FROM memories WHERE pipeline_id = ? AND scope = ? ORDER BY id DESC LIMIT ?
		)
	`, pipelineID, scope, pipelineID, scope, limit)
	if err != nil {
		return fmt.Errorf("could not cap the memories of %q: %w", scope, err)
	}

	return nil
}

// ListMemories is scope's entries, newest first.
func (s *Store) ListMemories(ctx context.Context, scope string, limit int) ([]store.Memory, error) {
	return collect(ctx, s.reads, "the memories of "+scope, memoryColumns+`
		WHERE pipeline_id = ? AND scope = ?
		ORDER BY id DESC LIMIT ?
	`, []any{s.pipelineID, scope, rowLimit(limit)}, func(rows *sql.Rows) (store.Memory, error) {
		return scanMemory(rows)
	})
}

// Forget deletes one entry, and only when it is scope's.
func (s *Store) Forget(ctx context.Context, scope string, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM memories WHERE pipeline_id = ? AND scope = ? AND id = ?
	`, s.pipelineID, scope, id)
	if err != nil {
		return false, fmt.Errorf("could not forget memory %d of %q: %w", id, scope, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not forget memory %d of %q: %w", id, scope, err)
	}

	return affected > 0, nil
}

// ForgetScope deletes every entry of scope.
func (s *Store) ForgetScope(ctx context.Context, scope string) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM memories WHERE pipeline_id = ? AND scope = ?
	`, s.pipelineID, scope)
	if err != nil {
		return 0, fmt.Errorf("could not forget the memories of %q: %w", scope, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("could not forget the memories of %q: %w", scope, err)
	}

	return int(affected), nil
}

// MemoryScopes is every scope holding an entry, by name.
func (s *Store) MemoryScopes(ctx context.Context) ([]store.MemoryScope, error) {
	return collect(ctx, s.reads, "memory scopes", `
		SELECT scope, COUNT(*), MAX(created_at) FROM memories
		WHERE pipeline_id = ?
		GROUP BY scope ORDER BY scope
	`, []any{s.pipelineID}, func(rows *sql.Rows) (store.MemoryScope, error) {
		var scope store.MemoryScope

		return scope, rows.Scan(&scope.Scope, &scope.Entries, &scope.LastAt)
	})
}

const memoryColumns = `
	SELECT id, scope, text, COALESCE(run_id, ''), created_at FROM memories
`

func scanMemory(row rowScanner) (store.Memory, error) {
	var memory store.Memory

	err := row.Scan(&memory.ID, &memory.Scope, &memory.Text, &memory.RunID, &memory.CreatedAt)

	return memory, err //nolint:wrapcheck // every caller wraps with what it was reading
}
