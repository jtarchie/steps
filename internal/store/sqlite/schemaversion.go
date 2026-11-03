package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// checkSchemaVersion refuses a database this build cannot write to, and stamps
// one it just created. By the time it runs, upgrade has already brought any
// file from v0.1.0 on to schemaVersion, so what it refuses is a file older
// than that or newer than this build. What it adds is that somebody is TOLD.
//
// Without it the failure was silent and looked like success. The schema is
// CREATE TABLE IF NOT EXISTS, so an older database opens fine and simply lacks
// a column; every INSERT naming that column then fails, and the run-event sink
// only warns — a build went green having recorded nothing, and whoever ran it
// found out when they opened the web UI to an empty run. A green build that
// quietly lost its history is worse than a red one that says why.
//
// PRAGMA user_version rather than a table: it lives in the file header, costs
// no row, and cannot itself be the thing that is missing.
func checkSchemaVersion(ctx context.Context, db executor, path string) error {
	found, err := readSchemaVersion(ctx, db, path)
	if err != nil {
		return err
	}

	// Zero is a database created before this check existed, which is every
	// database older than it — and every one of those predates at least this
	// column. Stamping instead of refusing would bless exactly the stale files
	// the check is for, so the only zero that is accepted is one this open
	// just created, which the DDL above has already filled in.
	if found == schemaVersion {
		return nil
	}

	if found == 0 && freshlyCreated(ctx, db) {
		return writeVersion(ctx, db, path)
	}

	return schemaVersionError(path, found)
}

// readSchemaVersion reports what schema the file at path was written by.
func readSchemaVersion(ctx context.Context, db executor, path string) (int, error) {
	var found int

	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&found)
	if err != nil {
		return 0, fmt.Errorf("could not read the schema version of %q: %w", path, err)
	}

	return found, nil
}

// schemaVersionError is the one message a file this build cannot use gets,
// whether the caller meant to write to it or only to read it — a reader of a
// stale file is told the same thing, because the answer is the same and
// because a read that silently returned nothing is the failure mode this
// whole check exists for.
func schemaVersionError(path string, found int) error {
	switch {
	case found > schemaVersion:
		return fmt.Errorf("%w: %s is at schema %d, written by a newer steps than this one (%d); use that steps or a later one",
			store.ErrSchemaVersion, path, found, schemaVersion)
	case found >= oldestUpgradable:
		return fmt.Errorf("%w: %s is at schema %d and this steps writes %d; the next steps command that writes to it (steps web, steps run) upgrades it, keeping a copy beside it",
			store.ErrSchemaVersion, path, found, schemaVersion)
	}

	return fmt.Errorf("%w: %s is at schema %d, older than steps v0.1.0 (%d), and has no upgrade path; delete the file and run again, which loses that pipeline's run history and cache but nothing else",
		store.ErrSchemaVersion, path, found, oldestUpgradable)
}

// freshlyCreated reports that this open made the database, rather than
// inheriting one an older build left. A database with no run and no node has
// nothing to lose by being stamped.
func freshlyCreated(ctx context.Context, db executor) bool {
	var rows int

	err := db.QueryRowContext(ctx,
		"SELECT (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM nodes)").Scan(&rows)

	return err == nil && rows == 0
}

func writeVersion(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, path string) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	if err != nil {
		return fmt.Errorf("could not stamp the schema version of %q: %w", path, err)
	}

	return nil
}
