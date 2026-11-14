package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// oldestUpgradable is the schema steps v0.1.0 shipped. Anything older predates migrations, and its answer is still DROP SCHEMA.
const oldestUpgradable = 14

// migrations[i] takes a schema at oldestUpgradable+i to the next version. APPEND ONLY: a step that has reached main has run on somebody's database, so a mistake in one is fixed by the next. TestV010UpgradesToTheFreshSchema holds the sum of them to the schema a fresh database gets.
//
//nolint:gochecknoglobals // a fact about the schema, not state
var migrations = []string{
	`ALTER TABLE run_placements DROP COLUMN fstype, DROP COLUMN fs_free`,
	`CREATE TABLE credentials (
    pipeline_id BIGINT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sealed      BYTEA NOT NULL,
    PRIMARY KEY (pipeline_id, name)
)`,
	`ALTER TABLE run_placements ADD COLUMN volume_bytes BIGINT`,
	`ALTER TABLE run_steps ADD COLUMN hash TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE run_steps
    ADD COLUMN visit INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN outcome TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT run_steps_pkey,
    ADD PRIMARY KEY (run_id, build_id, step_index, visit)`,
	`CREATE INDEX idx_run_inputs_version ON run_inputs(resource_name, version_json)`,
	`CREATE TABLE pause_all (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    paused_at TIMESTAMPTZ NOT NULL
)`,
	`CREATE TABLE run_step_outputs (
    run_id  TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id BIGINT NOT NULL,
    n       INTEGER NOT NULL,
    seq     INTEGER NOT NULL,
    chunk   BYTEA NOT NULL,
    PRIMARY KEY (run_id, step_id, n, seq)
)`,
	`ALTER TABLE trigger_queue ADD COLUMN pin TEXT;
DROP INDEX idx_trigger_queue_pending_job;
CREATE UNIQUE INDEX idx_trigger_queue_pending_job
    ON trigger_queue(pipeline_id, job_name) WHERE status = 'pending' AND rerun_of IS NULL AND run_id IS NULL AND pin IS NULL;
CREATE UNIQUE INDEX idx_trigger_queue_pending_pin
    ON trigger_queue(pipeline_id, job_name, pin) WHERE status = 'pending' AND pin IS NOT NULL AND run_id IS NULL`,
}

// migrate brings a schema from found to schemaVersion inside the caller's transaction, which also holds the schema lock — so a failed step leaves it exactly as it was, Postgres DDL being transactional.
func migrate(ctx context.Context, tx *sql.Tx, found int) error {
	for version := found; version < schemaVersion; version++ {
		_, err := tx.ExecContext(ctx, migrations[version-oldestUpgradable])
		if err != nil {
			return fmt.Errorf("could not upgrade the schema from %d to %d (it is unchanged): %w", version, version+1, err)
		}
	}

	_, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = $1`, schemaVersion)
	if err != nil {
		return fmt.Errorf("could not stamp the schema version: %w", err)
	}

	return nil
}
