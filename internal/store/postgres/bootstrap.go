package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// initDB creates the schema on first sight, upgrades one steps v0.1.0 or later
// wrote, refuses any other, and registers the pipeline — all in one transaction, so a refused or failed
// open leaves nothing behind (Postgres DDL is transactional).
//
// The DDL runs only when the schema is ABSENT. A matching schema costs one
// read, not sixty statements over the network, and needs no CREATE privilege,
// so a role that may only use the tables can open it.
//
// The advisory lock serializes two first opens: without it both see no
// schema_version and both run the DDL, and the loser fails on a duplicate.
func initDB(ctx context.Context, conn connection, pipelineName string) (int64, error) {
	var id int64

	err := inSchema(ctx, conn, func(tx *sql.Tx) error {
		var err error

		id, err = registerPipeline(ctx, tx, pipelineName)

		return err
	})
	if err != nil {
		return 0, err
	}

	return id, nil
}

// inSchema runs fn in a transaction against a schema it has created or checked.
func inSchema(ctx context.Context, conn connection, fn func(tx *sql.Tx) error) error {
	err := inTx(ctx, conn.db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, classSchema)
		if err != nil {
			return err //nolint:wrapcheck // inTx's caller names the database
		}

		found, present, err := readSchemaVersion(ctx, tx, conn.schema)
		if err != nil {
			return err
		}

		switch {
		case !present:
			err = createSchema(ctx, tx, conn.schema)
		case found >= oldestUpgradable && found < schemaVersion:
			err = migrate(ctx, tx, found)
		case found != schemaVersion:
			err = schemaVersionError(conn, found)
		}

		if err != nil {
			return err
		}

		return fn(tx)
	})
	if err != nil {
		return fmt.Errorf("could not open state db %s: %w", conn.description, describe(err))
	}

	return nil
}

func createSchema(ctx context.Context, tx *sql.Tx, schemaName string) error {
	var exists bool

	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schemaName).Scan(&exists)
	if err != nil {
		return fmt.Errorf("could not read the schemas: %w", err)
	}

	if !exists {
		_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+quote(schemaName))
		if err != nil {
			return fmt.Errorf("could not create schema %s: %w", schemaName, err)
		}
	}

	_, err = tx.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("could not create the tables: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES ($1)`, schemaVersion)
	if err != nil {
		return fmt.Errorf("could not stamp the schema version: %w", err)
	}

	return nil
}

// readSchemaVersion reports the stamp, and whether there is one at all. An
// older steps schema cannot lack the table — it was there from the first —
// so absent means this schema has never been written.
//
// It also refuses a session the pinned search_path never reached. A pooler
// told to drop the startup parameter (PgBouncer's ignore_startup_parameters,
// the usual answer to its "unsupported startup parameter") leaves the
// server's default "$user", public — and every unqualified name here would
// then create and write steps' tables in public, beside another
// application's. Same round trip as the stamp check, so it costs nothing.
func readSchemaVersion(ctx context.Context, db executor, schemaName string) (int, bool, error) {
	var (
		present bool
		path    string
	)

	err := db.QueryRowContext(ctx,
		`SELECT to_regclass('schema_version') IS NOT NULL, current_setting('search_path')`).Scan(&present, &path)
	if err != nil {
		return 0, false, describe(err)
	}

	if path != quote(schemaName) {
		return 0, false, fmt.Errorf("the session's search_path is %q, not %s: something between steps and the server dropped the one steps sets (a pooler's ignore_startup_parameters?), and its tables would land in the wrong schema; add search_path to PgBouncer's track_extra_parameters instead", path, quote(schemaName))
	}

	if !present {
		return 0, false, nil
	}

	var found int

	err = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&found)
	if err != nil {
		return 0, false, fmt.Errorf("could not read the schema version: %w", err)
	}

	return found, true, nil
}

func schemaVersionError(conn connection, found int) error {
	switch {
	case found > schemaVersion:
		return fmt.Errorf("%w: %s is at schema %d, written by a newer steps than this one (%d); use that steps or a later one",
			store.ErrSchemaVersion, conn.description, found, schemaVersion)
	case found >= oldestUpgradable:
		return fmt.Errorf("%w: %s is at schema %d and this steps writes %d; the next steps command that writes to it (steps web, steps run) upgrades it",
			store.ErrSchemaVersion, conn.description, found, schemaVersion)
	}

	return fmt.Errorf("%w: %s is at schema %d, older than steps v0.1.0 (%d), and has no upgrade path; `DROP SCHEMA %s CASCADE` and run again, which loses steps' run history and cache in that schema and nothing else",
		store.ErrSchemaVersion, conn.description, found, oldestUpgradable, quote(conn.schema))
}
