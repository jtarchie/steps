package store

// Foreign-key enforcement, which SQLite leaves off unless a connection asks.

import (
	"context"
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

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	var enabled int

	err := store.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled)
	if err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}

	if enabled != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 — a REFERENCES clause would be ignored", enabled)
	}

	_, err = store.db.ExecContext(ctx, `
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
	_, err = store.db.ExecContext(ctx, `INSERT INTO fk_child (id, parent_id) VALUES ('c2', 'nope')`)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("inserting an orphan: err = %v, want a foreign-key violation", err)
	}

	// And deleting the parent takes the child with it.
	_, err = store.db.ExecContext(ctx, `DELETE FROM fk_parent WHERE id = 'p1'`)
	if err != nil {
		t.Fatalf("deleting parent: %v", err)
	}

	var children int

	err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fk_child`).Scan(&children)
	if err != nil {
		t.Fatal(err)
	}

	if children != 0 {
		t.Errorf("%d child rows survived the parent's deletion, want 0 — ON DELETE CASCADE did nothing", children)
	}
}
