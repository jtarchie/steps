package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

//go:embed testdata/v0.1.0.sql
var v010 string

// loadV010 is a database steps v0.1.0 wrote, rows and all.
func loadV010(t *testing.T) string {
	t.Helper()

	rawURL := newDatabase(t)

	conn, err := rawDB(t, rawURL).Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = conn.Close() }()

	_, err = conn.ExecContext(t.Context(), "CREATE SCHEMA steps; SET search_path = steps;\n"+v010)
	if err != nil {
		t.Fatalf("loading the fixture: %v", err)
	}

	return rawURL
}

// TestV010UpgradesToTheFreshSchema is what every migration answers to: a schema steps v0.1.0 wrote, opened by this build, is the schema a fresh database gets, with its rows still in it.
func TestV010UpgradesToTheFreshSchema(t *testing.T) {
	t.Parallel()

	old := loadV010(t)
	st := openStore(t, old, "app")

	placements, err := st.RunPlacements(context.Background(), "R1")
	if err != nil || len(placements) != 1 || placements[0].Address != "ssh://box" || placements[0].BytesSent != 4096 {
		t.Errorf("v0.1.0's placement after the upgrade = %+v (%v), want its row kept", placements, err)
	}

	fresh := newDatabase(t)
	openStore(t, fresh, "app")

	if got, want := schemaOf(t, old), schemaOf(t, fresh); !slices.Equal(got, want) {
		t.Errorf("the upgraded schema is not the fresh one:\nupgraded only: %v\nfresh only:    %v", missingFrom(got, want), missingFrom(want, got))
	}
}

// TestEveryVersionHasItsMigration: schemaVersion and the step list are two facts about one thing.
func TestEveryVersionHasItsMigration(t *testing.T) {
	t.Parallel()

	if got := oldestUpgradable + len(migrations); got != schemaVersion {
		t.Errorf("%d migrations from %d reach %d, but schemaVersion is %d", len(migrations), oldestUpgradable, got, schemaVersion)
	}
}

// TestOpenReaderLeavesTheUpgradeToAWriter: a command that only reads must not rewrite the schema, and says what will.
func TestOpenReaderLeavesTheUpgradeToAWriter(t *testing.T) {
	t.Parallel()

	rawURL := loadV010(t)

	_, err := OpenReader(rawURL)
	if !errors.Is(err, store.ErrSchemaVersion) || !strings.Contains(err.Error(), "upgrades it") {
		t.Fatalf("OpenReader = %v, want the upgrade left to a writer", err)
	}

	if got := queryInt(t, rawDB(t, rawURL), `SELECT version FROM steps.schema_version`); got != oldestUpgradable {
		t.Errorf("schema_version after a read = %d, want it untouched", got)
	}
}

// schemaOf is the steps schema as the catalog describes it — columns, constraints, indexes, the stamp — rather than as the DDL was written.
func schemaOf(t *testing.T, rawURL string) []string {
	t.Helper()

	db := rawDB(t, rawURL)

	facts := queryStrings(t, db, `
		SELECT 'column ' || table_name || '.' || column_name || ' ' || data_type || ' null=' || is_nullable || ' default=' || COALESCE(column_default, '') || ' identity=' || is_identity
			FROM information_schema.columns WHERE table_schema = 'steps'
		UNION ALL SELECT 'constraint ' || conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
			FROM pg_constraint WHERE connamespace = 'steps'::regnamespace
		UNION ALL SELECT 'index ' || indexname || ' ' || indexdef FROM pg_indexes WHERE schemaname = 'steps'
		UNION ALL SELECT 'version ' || version FROM steps.schema_version`)

	slices.Sort(facts)

	return facts
}

func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var value string

		err = rows.Scan(&value)
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, value)
	}

	err = rows.Err()
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func missingFrom(have, other []string) []string {
	return slices.DeleteFunc(slices.Clone(have), func(fact string) bool { return slices.Contains(other, fact) })
}
