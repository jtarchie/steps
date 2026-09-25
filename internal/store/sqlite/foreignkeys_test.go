package sqlite

// Foreign-key enforcement, which SQLite leaves off unless a connection asks.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestForeignKeysAreEnforced proves the pragma is actually on.
//
// It matters more than it looks: SQLite parses and then IGNORES a REFERENCES
// clause when enforcement is off, so a constraint added later would be
// decorative and every test of its cascade would pass for the wrong reason —
// the rows would be gone because nothing ever created them, or still there
// with nobody noticing. This asserts the setting itself, against a throwaway
// pair of tables, so it keeps holding whatever the real schema grows.
func TestForeignKeysAreEnforced(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	var enabled int

	err := st.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled)
	if err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}

	if enabled != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 — a REFERENCES clause would be ignored", enabled)
	}

	_, err = st.db.ExecContext(ctx, `
		CREATE TABLE fk_parent (id TEXT PRIMARY KEY);
		CREATE TABLE fk_child (
			id        TEXT PRIMARY KEY,
			parent_id TEXT NOT NULL REFERENCES fk_parent(id) ON DELETE CASCADE
		);
		INSERT INTO fk_parent (id) VALUES ('p1');
		INSERT INTO fk_child (id, parent_id) VALUES ('c1', 'p1');
	`)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// A child with no parent is refused rather than accepted.
	_, err = st.db.ExecContext(ctx, `INSERT INTO fk_child (id, parent_id) VALUES ('c2', 'nope')`)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("inserting an orphan: err = %v, want a foreign-key violation", err)
	}

	// And deleting the parent takes the child with it.
	_, err = st.db.ExecContext(ctx, `DELETE FROM fk_parent WHERE id = 'p1'`)
	if err != nil {
		t.Fatalf("deleting parent: %v", err)
	}

	var children int

	err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fk_child`).Scan(&children)
	if err != nil {
		t.Fatal(err)
	}

	if children != 0 {
		t.Errorf("%d child rows survived the parent's deletion, want 0 — ON DELETE CASCADE did nothing", children)
	}
}

// TestEveryForeignKeyIsIndexed holds each child key to an index that leads with its columns.
//
// SQLite never indexes a child key on its own, and without one every delete of a parent row scans the whole child table to find what to cascade, set null or restrict — inside the write transaction, once per parent row. Measured on job_versions: pruning 2000 versions took 3.5s unindexed and 26ms indexed. The primary key or a UNIQUE counts, since either is an index.
func TestEveryForeignKeyIsIndexed(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = st.Close() }()

	ctx := t.Context()

	type childKey struct{ table, columns string }

	keys, err := collect(ctx, st.db, "foreign keys", `
		SELECT m.name, group_concat(fk."from", ',')
		FROM sqlite_schema m, pragma_foreign_key_list(m.name) fk
		WHERE m.type = 'table'
		GROUP BY m.name, fk.id`, nil, func(rows *sql.Rows) (childKey, error) {
		var key childKey

		return key, rows.Scan(&key.table, &key.columns)
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(keys) == 0 {
		t.Fatal("no foreign keys found; the schema query is broken")
	}

	for _, key := range keys {
		if !leadingIndex(ctx, t, st, key.table, strings.Split(key.columns, ",")) {
			t.Errorf("%s(%s) is a foreign key with no index leading with it; deleting a parent row scans %s",
				key.table, key.columns, key.table)
		}
	}
}

// leadingIndex reports whether some index on table has exactly columns as its first len(columns) columns, in any order — the planner satisfies equality on all of them either way.
func leadingIndex(ctx context.Context, t *testing.T, st *Store, table string, columns []string) bool {
	t.Helper()

	want := map[string]bool{}
	for _, c := range columns {
		want[c] = true
	}

	rows, err := st.db.QueryContext(ctx, `
		SELECT il.name, ii.name
		FROM pragma_index_list(?) il, pragma_index_info(il.name) ii
		WHERE il.partial = 0 AND ii.seqno < ?
		ORDER BY il.name, ii.seqno`, table, len(columns))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	matched := map[string]int{}

	for rows.Next() {
		var index, column string

		err = rows.Scan(&index, &column)
		if err != nil {
			t.Fatal(err)
		}

		if want[column] {
			matched[index]++
		}
	}

	err = rows.Err()
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range matched {
		if n == len(columns) {
			return true
		}
	}

	return false
}
