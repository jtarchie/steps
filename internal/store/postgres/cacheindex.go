package postgres

// The cache index (#259), machine-wide on purpose; see the sqlite schema.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// CacheEntry implements store.Blobs.
func (s *Store) CacheEntry(ctx context.Context, kind store.CacheKind, key string) (map[string]store.CachedTree, error) {
	rows, err := collect(ctx, s.db, "cache entry",
		`SELECT name, tree_name, digest, holder FROM cache_entry_trees WHERE kind = $1 AND key = $2`,
		[]any{string(kind), key}, scanCachedTree)
	if err != nil {
		return nil, err
	}

	trees := make(map[string]store.CachedTree, len(rows))
	for _, row := range rows {
		trees[row.name] = row.tree
	}

	return trees, nil
}

type namedTree struct {
	name string
	tree store.CachedTree
}

func scanCachedTree(rows *sql.Rows) (namedTree, error) {
	var row namedTree

	err := rows.Scan(&row.name, &row.tree.Name, &row.tree.Digest, &row.tree.Holder)

	return row, err //nolint:wrapcheck // collect names what it was reading
}

// RecordCacheEntry takes classCache: its prune reads which entries survive before deleting the rest, and the index is shared by every pipeline, so the pipeline lock would not serialize two of them.
func (s *Store) RecordCacheEntry(ctx context.Context, kind store.CacheKind, key string, trees map[string]store.CachedTree, keep int) ([]string, error) {
	var gone []string

	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, classCache)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		before, err := localTrees(ctx, tx)
		if err != nil {
			return err
		}

		err = replaceCacheEntry(ctx, tx, kind, key, trees, keep)
		if err != nil {
			return err
		}

		after, err := localTrees(ctx, tx)
		if err != nil {
			return err
		}

		for digest := range before {
			if !after[digest] {
				gone = append(gone, digest)
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not record cache entry: %w", err)
	}

	return gone, nil
}

func replaceCacheEntry(ctx context.Context, tx *sql.Tx, kind store.CacheKind, key string, trees map[string]store.CachedTree, keep int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO cache_entries (kind, key, used_at) VALUES ($1, $2, $3)
		ON CONFLICT (kind, key) DO UPDATE SET used_at = excluded.used_at`, string(kind), key, now())
	if err != nil {
		return err //nolint:wrapcheck // wrapped by RecordCacheEntry
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM cache_entry_trees WHERE kind = $1 AND key = $2`, string(kind), key)
	if err != nil {
		return err //nolint:wrapcheck // wrapped by RecordCacheEntry
	}

	for name, tree := range trees {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO cache_entry_trees (kind, key, name, tree_name, digest, holder) VALUES ($1, $2, $3, $4, $5, $6)`,
			string(kind), key, name, tree.Name, tree.Digest, tree.Holder)
		if err != nil {
			return err //nolint:wrapcheck // wrapped by RecordCacheEntry
		}
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM cache_entries WHERE kind = $1 AND key NOT IN (
		SELECT key FROM cache_entries WHERE kind = $1 ORDER BY used_at DESC, key LIMIT $2)`, string(kind), keep)

	return err //nolint:wrapcheck // wrapped by RecordCacheEntry
}

// localTrees is every digest an entry names in this machine's store; the index is bounded by count, so reading all of it is cheap.
func localTrees(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT digest FROM cache_entry_trees WHERE holder = ''`)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by RecordCacheEntry
	}

	defer func() { _ = rows.Close() }()

	digests := map[string]bool{}

	for rows.Next() {
		var digest string

		err = rows.Scan(&digest)
		if err != nil {
			return nil, err //nolint:wrapcheck // wrapped by RecordCacheEntry
		}

		digests[digest] = true
	}

	return digests, rows.Err() //nolint:wrapcheck // wrapped by RecordCacheEntry
}

// TouchCacheEntry implements store.Blobs.
func (s *Store) TouchCacheEntry(ctx context.Context, kind store.CacheKind, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cache_entries SET used_at = $1 WHERE kind = $2 AND key = $3`, now(), string(kind), key)
	if err != nil {
		return fmt.Errorf("could not touch cache entry: %w", err)
	}

	return nil
}
