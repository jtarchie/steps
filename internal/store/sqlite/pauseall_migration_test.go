package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestThePauseAllStepCreatesTheTable runs the migration step by itself: upgrade also replays the schema's IF NOT EXISTS DDL, which would hide a step that did nothing.
func TestThePauseAllStepCreatesTheTable(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	err = migrations[34-oldestUpgradable](ctx, tx)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO pause_all (singleton, paused_at) VALUES (1, 'now')`)
	if err != nil {
		t.Fatalf("the step to 35 left no usable pause_all: %v", err)
	}
}
