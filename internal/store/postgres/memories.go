package postgres

// memories: what an agent step keeps about a scope from one run to the next.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// Remember files the memory, or finds it already filed, and applies the cap.
// Under write() because the cap reads the scope's entries before deleting
// past them, and two rememberers under READ COMMITTED would each keep a view
// the other just changed.
func (s *Store) Remember(ctx context.Context, memory store.Memory, limit int) (store.Memory, bool, error) {
	err := store.CheckMemory(memory)
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	var (
		stored store.Memory
		added  bool
	)

	err = s.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO memories (pipeline_id, scope, text, run_id, created_at)
			VALUES ($1, $2, $3, (SELECT id FROM runs WHERE id = $4 AND pipeline_id = $1), $5)
			ON CONFLICT (pipeline_id, scope, text) DO NOTHING
		`, s.pipelineID, memory.Scope, memory.Text, memory.RunID, now())
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the scope
		}

		affected, err := result.RowsAffected()
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the scope
		}

		added = affected > 0

		err = evictMemories(ctx, tx, s.pipelineID, memory.Scope, limit)
		if err != nil {
			return err
		}

		stored, err = scanMemory(tx.QueryRowContext(ctx, memoryColumns+`
			WHERE pipeline_id = $1 AND scope = $2 AND text = $3
		`, s.pipelineID, memory.Scope, memory.Text))

		return err
	})
	if err != nil {
		return store.Memory{}, false, fmt.Errorf("could not remember for %q: %w", memory.Scope, err)
	}

	return stored, added, nil
}

// evictMemories keeps a scope's newest limit entries, by identity order.
func evictMemories(ctx context.Context, tx *sql.Tx, pipelineID int64, scope string, limit int) error {
	if limit < 0 {
		limit = store.DefaultMemoryEntries
	}

	if limit == 0 {
		return nil
	}

	_, err := tx.ExecContext(ctx, `
		DELETE FROM memories
		WHERE pipeline_id = $1 AND scope = $2 AND id NOT IN (
			SELECT id FROM memories WHERE pipeline_id = $1 AND scope = $2 ORDER BY id DESC LIMIT $3
		)
	`, pipelineID, scope, limit)
	if err != nil {
		return fmt.Errorf("could not cap the memories of %q: %w", scope, err)
	}

	return nil
}

// ListMemories is scope's entries, newest first.
func (s *Store) ListMemories(ctx context.Context, scope string, limit int) ([]store.Memory, error) {
	return collect(ctx, s.db, "the memories of "+scope, memoryColumns+`
		WHERE pipeline_id = $1 AND scope = $2
		ORDER BY id DESC LIMIT $3
	`, []any{s.pipelineID, scope, rowLimit(limit)}, func(rows *sql.Rows) (store.Memory, error) {
		return scanMemory(rows)
	})
}

// Forget deletes one entry, and only when it is scope's.
func (s *Store) Forget(ctx context.Context, scope string, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM memories WHERE pipeline_id = $1 AND scope = $2 AND id = $3
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
		DELETE FROM memories WHERE pipeline_id = $1 AND scope = $2
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
	return collect(ctx, s.db, "memory scopes", `
		SELECT scope, COUNT(*), MAX(created_at) FROM memories
		WHERE pipeline_id = $1
		GROUP BY scope ORDER BY scope
	`, []any{s.pipelineID}, func(rows *sql.Rows) (store.MemoryScope, error) {
		var (
			scope store.MemoryScope
			last  time.Time
		)

		err := rows.Scan(&scope.Scope, &scope.Entries, &last)
		scope.LastAt = last.UTC().Format(sortableNano)

		return scope, err //nolint:wrapcheck // collect wraps with what it was reading
	})
}

const memoryColumns = `
	SELECT id, scope, text, COALESCE(run_id, ''), created_at FROM memories
`

// scanMemory renders created_at in the sqlite driver's sortableNano, the
// layout it stores this table's in.
func scanMemory(row rowScanner) (store.Memory, error) {
	var (
		memory  store.Memory
		created time.Time
	)

	err := row.Scan(&memory.ID, &memory.Scope, &memory.Text, &memory.RunID, &created)
	memory.CreatedAt = created.UTC().Format(sortableNano)

	return memory, err //nolint:wrapcheck // every caller wraps with what it was reading
}
