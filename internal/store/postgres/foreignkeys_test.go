package postgres

import (
	"testing"
)

// TestEveryForeignKeyDeclaresItsDeleteAction holds this driver to the repo's
// rule: a reference says what happens when its parent goes. Postgres's
// default, NO ACTION, is a refusal nobody chose.
func TestEveryForeignKeyDeclaresItsDeleteAction(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	rows, err := rawDB(t, rawURL).QueryContext(t.Context(), `
		SELECT conrelid::regclass::text, conname FROM pg_constraint
		WHERE contype = 'f' AND connamespace = 'steps'::regnamespace AND confdeltype = 'a'
	`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var table, name string

		err = rows.Scan(&table, &name)
		if err != nil {
			t.Fatal(err)
		}

		t.Errorf("%s.%s declares no ON DELETE action", table, name)
	}

	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}

	declared := queryInt(t, rawDB(t, rawURL),
		`SELECT COUNT(*) FROM pg_constraint WHERE contype = 'f' AND connamespace = 'steps'::regnamespace`)
	if declared < 20 {
		t.Fatalf("found %d foreign keys; the query has stopped reading the schema, and none would pass for all declared", declared)
	}
}

// TestEveryForeignKeyIsIndexed: Postgres enforces a reference without an
// index on the child, and then every cascade — retention's, a pipeline
// delete's — scans the child table once per parent row it removes.
func TestEveryForeignKeyIsIndexed(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	rows, err := rawDB(t, rawURL).QueryContext(t.Context(), `
		SELECT c.conrelid::regclass::text, c.conname FROM pg_constraint c
		WHERE c.contype = 'f' AND c.connamespace = 'steps'::regnamespace
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_index i
		      WHERE i.indrelid = c.conrelid
		        AND (string_to_array(i.indkey::text, ' ')::int2[])[1:cardinality(c.conkey)] @> c.conkey
		        AND c.conkey @> (string_to_array(i.indkey::text, ' ')::int2[])[1:cardinality(c.conkey)]
		  )
	`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var table, name string

		err = rows.Scan(&table, &name)
		if err != nil {
			t.Fatal(err)
		}

		t.Errorf("%s.%s has no index leading with its columns", table, name)
	}

	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
}

// TestTheDeliberateNonReferencesStayThatWay pins the three columns that name
// a row and must not reference it, for the reasons the schema gives. Adding
// either of the first two makes every block step fail to record.
func TestTheDeliberateNonReferencesStayThatWay(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	db := rawDB(t, rawURL)

	for table, column := range map[string]string{
		"nodes": "parent_hash", "job_runs": "root_hash", "run_events": "parent_step_id",
	} {
		var references int

		err := db.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM pg_constraint c
			JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
			WHERE c.contype = 'f' AND c.conrelid = ('steps.' || $1)::regclass AND a.attname = $2
		`, table, column).Scan(&references)
		if err != nil {
			t.Fatal(err)
		}

		if references != 0 {
			t.Errorf("%s.%s declares a foreign key; it must not (see the sqlite schema)", table, column)
		}
	}
}
