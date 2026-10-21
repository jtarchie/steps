package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// oldestUpgradable is the schema steps v0.1.0 shipped. Anything older predates migrations, and its answer is still deleting the file.
const oldestUpgradable = 28

// migrations[i] takes a file at oldestUpgradable+i to the next version. APPEND ONLY: a step that has reached main has run on somebody's database, so a mistake in one is fixed by the next. TestV010UpgradesToTheFreshSchema holds the sum of them to the schema a fresh file gets.
//
//nolint:gochecknoglobals // a fact about the schema, not state
var migrations = []func(ctx context.Context, tx *sql.Tx) error{
	execStep(`ALTER TABLE run_placements DROP COLUMN fstype;
ALTER TABLE run_placements DROP COLUMN fs_free;`),
	execStep(`CREATE TABLE credentials (
    pipeline_id INTEGER NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sealed      BLOB NOT NULL,
    PRIMARY KEY (pipeline_id, name)
);`),
}

func execStep(statements string) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, statements)

		return err //nolint:wrapcheck // upgrade names the step
	}
}

// upgrade brings a file an older steps wrote to schemaVersion, one committed transaction after a backup beside it. A file at the current version, a new one, and one too old to upgrade are left for checkSchemaVersion to judge. acquirePools calls it under poolsMu, so two handles opening one old file at once do not both back it up and migrate; another process is the one-steps-per-database rule's to prevent.
func upgrade(ctx context.Context, db *sql.DB, path string) error {
	found, err := readSchemaVersion(ctx, db, path)
	if err != nil || found < oldestUpgradable || found >= schemaVersion {
		return err
	}

	err = backup(ctx, db, path, found)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not upgrade state db %q: %w", path, err)
	}

	defer func() { _ = tx.Rollback() }()

	for version := found; version < schemaVersion; version++ {
		err = migrations[version-oldestUpgradable](ctx, tx)
		if err != nil {
			return fmt.Errorf("could not upgrade state db %q from schema %d to %d (it is unchanged; the copy taken before is %s): %w",
				path, version, version+1, backupPath(path, found), err)
		}
	}

	_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	if err != nil {
		return fmt.Errorf("could not stamp the schema version of %q: %w", path, err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not upgrade state db %q: %w", path, err)
	}

	return nil
}

// backup copies the file as it is before an upgrade, owner-only like the file itself, replacing the copy an earlier upgrade left: the newest is the one a failed upgrade needs.
func backup(ctx context.Context, db *sql.DB, path string, version int) error {
	// Not filepath.Glob: a path holding [ or * is a pattern there, refused or matching another file's backups.
	dir, base := filepath.Split(path)

	entries, err := os.ReadDir(cmp.Or(dir, "."))
	if err != nil {
		return fmt.Errorf("could not list the backups of %q: %w", path, err)
	}

	for _, entry := range entries {
		if !isBackupOf(entry.Name(), base) {
			continue
		}

		stale := filepath.Join(dir, entry.Name())

		err = os.Remove(stale)
		if err != nil {
			return fmt.Errorf("could not remove the older backup %q: %w", stale, err)
		}
	}

	target := backupPath(path, version)

	// VACUUM INTO takes an empty file, so this one is created first and keeps the mode it is created with.
	err = createPrivate(target)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, "VACUUM INTO ?", target)
	if err != nil {
		return fmt.Errorf("could not back up %q before upgrading it: %w", path, err)
	}

	return nil
}

// isBackupOf reports name is <base>.v<digits>.bak, so state.db's sweep leaves state.db.v2.db's backups alone.
func isBackupOf(name, base string) bool {
	version, ok := strings.CutPrefix(name, base+".v")
	if !ok {
		return false
	}

	version, ok = strings.CutSuffix(version, ".bak")
	if !ok {
		return false
	}

	_, err := strconv.Atoi(version)

	return err == nil
}

func backupPath(path string, version int) string {
	return fmt.Sprintf("%s.v%d.bak", path, version)
}
