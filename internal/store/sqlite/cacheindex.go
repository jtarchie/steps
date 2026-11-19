package sqlite

// The cache index (#259): which tree each step- and resource-cache entry names. Machine-wide on purpose; see the schema comment on cache_entries.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// CacheEntry implements store.Blobs.
func (s *Store) CacheEntry(ctx context.Context, kind store.CacheKind, key string) (map[string]store.CachedTree, error) {
	rows, err := collect(ctx, s.reads, "cache entry",
		`SELECT name, tree_name, digest, holder FROM cache_entry_trees WHERE kind = ? AND key = ?`,
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

// RecordCacheEntry implements store.Blobs.
func (s *Store) RecordCacheEntry(ctx context.Context, kind store.CacheKind, key string, trees map[string]store.CachedTree, keep int) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("could not record cache entry: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	before, err := localTrees(ctx, tx)
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO cache_entries (kind, key, used_at) VALUES (?, ?, ?)
		ON CONFLICT (kind, key) DO UPDATE SET used_at = excluded.used_at`, string(kind), key, nowNano())
	if err != nil {
		return nil, fmt.Errorf("could not record cache entry: %w", err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM cache_entry_trees WHERE kind = ? AND key = ?`, string(kind), key)
	if err != nil {
		return nil, fmt.Errorf("could not record cache entry: %w", err)
	}

	for name, tree := range trees {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO cache_entry_trees (kind, key, name, tree_name, digest, holder) VALUES (?, ?, ?, ?, ?, ?)`,
			string(kind), key, name, tree.Name, tree.Digest, tree.Holder)
		if err != nil {
			return nil, fmt.Errorf("could not record cache entry: %w", err)
		}
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM cache_entries WHERE kind = ? AND key NOT IN (
		SELECT key FROM cache_entries WHERE kind = ? ORDER BY used_at DESC LIMIT ?)`, string(kind), string(kind), keep)
	if err != nil {
		return nil, fmt.Errorf("could not prune the cache index: %w", err)
	}

	after, err := localTrees(ctx, tx)
	if err != nil {
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("could not record cache entry: %w", err)
	}

	return released(before, after), nil
}

// localTrees is every digest an entry names in this machine's store; the index is bounded by count, so reading all of it is cheap.
func localTrees(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT digest FROM cache_entry_trees WHERE holder = ''`)
	if err != nil {
		return nil, fmt.Errorf("could not read the cache index: %w", err)
	}

	defer func() { _ = rows.Close() }()

	digests := map[string]bool{}

	for rows.Next() {
		var digest string

		err = rows.Scan(&digest)
		if err != nil {
			return nil, fmt.Errorf("could not read the cache index: %w", err)
		}

		digests[digest] = true
	}

	return digests, rows.Err() //nolint:wrapcheck // a cursor error, named by the caller
}

func released(before, after map[string]bool) []string {
	var gone []string

	for digest := range before {
		if !after[digest] {
			gone = append(gone, digest)
		}
	}

	return gone
}

// TouchCacheEntry implements store.Blobs.
func (s *Store) TouchCacheEntry(ctx context.Context, kind store.CacheKind, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cache_entries SET used_at = ? WHERE kind = ? AND key = ?`, nowNano(), string(kind), key)
	if err != nil {
		return fmt.Errorf("could not touch cache entry: %w", err)
	}

	return nil
}
