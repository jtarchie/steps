package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

const versionRunsFilter = `EXPLAIN QUERY PLAN SELECT DISTINCT run_id, version_json FROM run_inputs WHERE resource_name = 'r' AND version_json IN (SELECT value FROM json_each('[]'))`

// TestRunInputsVersionIndexServesVersionRuns: VersionRuns filters run_inputs on (resource_name, version_json), which the (run_id, input_name) key cannot serve.
func TestRunInputsVersionIndexServesVersionRuns(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "fresh.db")

	st, err := OpenStore(path, "app")
	if err != nil {
		t.Fatal(err)
	}

	_ = st.Close()

	plan := strings.Join(queryStrings(t, path, versionRunsFilter), "\n")
	if !strings.Contains(plan, "idx_run_inputs_version") {
		t.Errorf("VersionRuns' filter on run_inputs does not use idx_run_inputs_version:\n%s", plan)
	}
}

// TestTheRunInputsIndexStepCreatesTheIndex runs the migration step by itself: upgrade also replays the schema's IF NOT EXISTS DDL, which would hide a step that did nothing.
func TestTheRunInputsIndexStepCreatesTheIndex(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	ctx := context.Background()

	_, err = db.ExecContext(ctx, `CREATE TABLE run_inputs (run_id TEXT, input_name TEXT, resource_name TEXT, version_json TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tx.Rollback() }()

	err = migrations[len(migrations)-1](ctx, tx)
	if err != nil {
		t.Fatal(err)
	}

	var found string

	err = tx.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_run_inputs_version'`).Scan(&found)
	if err != nil {
		t.Fatalf("the last migration step left no idx_run_inputs_version: %v", err)
	}
}
