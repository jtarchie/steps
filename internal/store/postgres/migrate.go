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
