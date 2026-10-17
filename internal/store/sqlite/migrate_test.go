package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

//go:embed testdata/v0.1.0.sql
var v010 string

// TestV010UpgradesToTheFreshSchema is what every migration answers to: a database steps v0.1.0 wrote, opened by this build, is the schema a fresh database gets, with its rows still in it and a copy of the original beside it.
func TestV010UpgradesToTheFreshSchema(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	old := filepath.Join(dir, "old.db")

	execFile(t, old, v010)

	st, err := OpenStore(old, "app")
	if err != nil {
		t.Fatalf("opening the v0.1.0 database: %v", err)
	}

	placements, err := st.RunPlacements(ctx, "R1")
	if err != nil || len(placements) != 1 || placements[0].Address != "ssh://box" || placements[0].BytesSent != 4096 {
		t.Errorf("v0.1.0's placement after the upgrade = %+v (%v), want its row kept", placements, err)
	}

	_ = st.Close()

	fresh := filepath.Join(dir, "fresh.db")

	st, err = OpenStore(fresh, "app")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	if got, want := schemaOf(t, old), schemaOf(t, fresh); !slices.Equal(got, want) {
		t.Errorf("the upgraded schema is not the fresh one:\nupgraded only: %v\nfresh only:    %v", missingFrom(got, want), missingFrom(want, got))
	}

	if broken := queryStrings(t, old, "PRAGMA foreign_key_check"); len(broken) > 0 {
		t.Errorf("the upgrade left rows breaking their foreign keys: %v", broken)
	}

	assertBackedUp(t, old+".v28.bak")
}

// assertBackedUp: the v0.1.0 file as it was, as private as the database.
func assertBackedUp(t *testing.T, backup string) {
	t.Helper()

	if got := queryStrings(t, backup, "PRAGMA user_version"); !slices.Equal(got, []string{"28"}) {
		t.Errorf("backup user_version = %v, want the v0.1.0 file as it was", got)
	}

	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("backup = %v (%v), want it as private as the database", info, err)
	}
}

// TestEveryVersionHasItsMigration: schemaVersion and the step list are two facts about one thing.
func TestEveryVersionHasItsMigration(t *testing.T) {
	t.Parallel()

	if got := oldestUpgradable + len(migrations); got != schemaVersion {
		t.Errorf("%d migrations from %d reach %d, but schemaVersion is %d", len(migrations), oldestUpgradable, got, schemaVersion)
	}
}

// TestOpenReaderLeavesTheUpgradeToAWriter: a command that only reads must not rewrite the file, and says what will.
func TestOpenReaderLeavesTheUpgradeToAWriter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	execFile(t, path, v010)

	_, err := OpenReader(path)
	if !errors.Is(err, store.ErrSchemaVersion) || !strings.Contains(err.Error(), "upgrades it") {
		t.Fatalf("OpenReader = %v, want the upgrade left to a writer", err)
	}

	if got := queryStrings(t, path, "PRAGMA user_version"); !slices.Equal(got, []string{"28"}) {
		t.Errorf("user_version after a read = %v, want it untouched", got)
	}
}

func execFile(t *testing.T, path, script string) {
	t.Helper()

	err := createPrivate(path)
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(context.Background(), script)
	if err != nil {
		t.Fatalf("loading the fixture: %v", err)
	}
}

func queryStrings(t *testing.T, path, query string) []string {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}

	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))

		for i := range values {
			pointers[i] = &values[i]
		}

		err = rows.Scan(pointers...)
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, strings.TrimSuffix(strings.TrimPrefix(fmt.Sprint(values), "["), "]"))
	}

	err = rows.Err()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// schemaOf is the file's schema as facts that do not depend on how it was written: each table's columns, keys and indexes, sorted. A migrated table keeps its CREATE text with the edits applied, so comparing that text would fail on whitespace and comments.
func schemaOf(t *testing.T, path string) []string {
	t.Helper()

	var facts []string

	for _, row := range queryStrings(t, path, "SELECT type, name, tbl_name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY name") {
		fields := strings.Fields(row)
		kind, name, table := fields[0], fields[1], fields[2]

		switch kind {
		case "table":
			for _, column := range queryStrings(t, path, fmt.Sprintf("SELECT name, type, \"notnull\", dflt_value, pk, hidden FROM pragma_table_xinfo('%s')", name)) {
				facts = append(facts, "column "+name+": "+column)
			}

			for _, key := range queryStrings(t, path, fmt.Sprintf(`SELECT "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list('%s')`, name)) {
				facts = append(facts, "foreign key "+name+": "+key)
			}
		case "index":
			unique := queryStrings(t, path, fmt.Sprintf(`SELECT "unique", partial FROM pragma_index_list('%s') WHERE name = '%s'`, table, name))
			columns := queryStrings(t, path, fmt.Sprintf("SELECT name, desc FROM pragma_index_xinfo('%s') WHERE key", name))
			facts = append(facts, fmt.Sprintf("index %s on %s %v: %v", name, table, unique, columns))
		default:
			facts = append(facts, kind+" "+name+" on "+table)
		}
	}

	facts = append(facts, "user_version "+strings.Join(queryStrings(t, path, "PRAGMA user_version"), ""))
	slices.Sort(facts)

	return facts
}

func missingFrom(have, other []string) []string {
	return slices.DeleteFunc(slices.Clone(have), func(fact string) bool { return slices.Contains(other, fact) })
}

// TestEveryWriterUpgradesAV010File: `steps web` with nobody signing in reaches the file first through Identity, and its reader refuses a file no writer has upgraded — so a writer that skipped the upgrade left the daemon unable to start on the very file it was told would be upgraded.
func TestEveryWriterUpgradesAV010File(t *testing.T) {
	t.Parallel()

	for name, open := range map[string]func(path string) error{
		"OpenStore": func(path string) error {
			st, err := OpenStore(path, "app")
			if err == nil {
				_ = st.Close()
			}

			return err
		},
		"OpenAccounts": func(path string) error {
			accounts, err := OpenAccounts(path)
			if err == nil {
				_ = accounts.Close()
			}

			return err
		},
		"Identity": func(path string) error {
			_, err := Identity(context.Background(), path)

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "state.db")
			execFile(t, path, v010)

			err := open(path)
			if err != nil {
				t.Fatalf("%s on a v0.1.0 file: %v", name, err)
			}

			if got := queryStrings(t, path, "PRAGMA user_version"); !slices.Equal(got, []string{strconv.Itoa(schemaVersion)}) {
				t.Errorf("user_version after %s = %v, want %d", name, got, schemaVersion)
			}
		})
	}
}

// TestABackupSweepTouchesOnlyItsOwnFile: the sweep that keeps only the newest copy must not refuse a path holding a glob metacharacter, nor take a neighbouring database's copies with it.
func TestABackupSweepTouchesOnlyItsOwnFile(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "team[a]")
	path := filepath.Join(dir, "state.db")

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	execFile(t, path, v010)

	stale, neighbour := path+".v27.bak", filepath.Join(dir, "state.db.v2.db.v28.bak")

	for _, file := range []string{stale, neighbour} {
		err = os.WriteFile(file, nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	st, err := OpenStore(path, "app")
	if err != nil {
		t.Fatalf("OpenStore under %q: %v", dir, err)
	}

	_ = st.Close()

	_, err = os.Stat(stale)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the older backup of this file survived: %v", err)
	}

	_, err = os.Stat(neighbour)
	if err != nil {
		t.Errorf("a neighbouring database's backup was removed: %v", err)
	}

	assertBackedUp(t, path+".v28.bak")
}
