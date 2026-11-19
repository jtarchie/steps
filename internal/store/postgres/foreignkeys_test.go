package postgres

import (
	"strings"
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

// The five columns the root CLAUDE.md names: each holds another row's key and must not declare it, for the reasons the sqlite schema gives.
var deliberateNonReferences = map[string]bool{
	"nodes.parent_hash":         true,
	"job_runs.root_hash":        true,
	"run_events.parent_step_id": true,
	"agent_usage.step_id":       true,
	"run_steps.hash":            true,
}

// Named like a reference, holding no other table's key, so the foreign-key rule has nothing to say about them.
var keylessColumns = map[string]string{
	"node_content.content_hash":    "the table's own primary key",
	"sessions.token_hash":          "the table's own primary key",
	"oauth_tokens.token_hash":      "the table's own primary key",
	"resource_configs.config_hash": "a digest of a resource's type and source, compared, never joined",
	"job_versions.build_id":        "the build label inside a run (\"<run>#0\"), no table's key",
	"run_steps.build_id":           "the build label inside a run (\"<run>#0\"), no table's key",
	"run_events.step_id":           "a step's number within its run, which every event of the step repeats",
	"run_step_outputs.step_id":     "run_events.step_id, which is no key; the run is the reference",
	"run_placements.instance_id":   "a cloud provider's instance id",
	"users.account_id":             "the sign-in provider's account id",
}

// The rule's reach is derived from the catalog, not a hand list: every *_id/*_hash column references, is one of the five (which must not), or is named as holding no key.
func TestEveryKeyColumnDeclaresItsReference(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	rows, err := rawDB(t, rawURL).QueryContext(t.Context(), `
		SELECT r.relname, a.attname, EXISTS (
		    SELECT 1 FROM pg_constraint c
		    WHERE c.contype = 'f' AND c.conrelid = a.attrelid AND a.attnum = ANY(c.conkey)
		)
		FROM pg_attribute a JOIN pg_class r ON r.oid = a.attrelid
		WHERE r.relkind = 'r' AND r.relnamespace = 'steps'::regnamespace AND a.attnum > 0 AND NOT a.attisdropped
	`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	seen := map[string]bool{}

	for rows.Next() {
		var (
			table, column string
			references    bool
		)

		err = rows.Scan(&table, &column, &references)
		if err != nil {
			t.Fatal(err)
		}

		name := table + "." + column
		seen[name] = true

		if problem := keyColumnProblem(name, column, references); problem != "" {
			t.Error(problem)
		}
	}

	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}

	if len(seen) < 100 {
		t.Fatalf("the walk found %d columns; it has stopped reading the schema, and an empty walk passes everything", len(seen))
	}

	for _, name := range staleListings(seen) {
		t.Errorf("%s is listed, but the schema no longer has it; drop it from the list", name)
	}
}

// keyColumnProblem is what is wrong with one column under the rule, or "".
func keyColumnProblem(name, column string, references bool) string {
	_, keyless := keylessColumns[name]

	switch {
	case (deliberateNonReferences[name] || keyless) && references:
		return name + " declares a foreign key but is listed as holding none"
	case deliberateNonReferences[name], keyless, references:
		return ""
	case strings.HasSuffix(column, "_id"), strings.HasSuffix(column, "_hash"):
		return name + " declares no REFERENCES; add one with an ON DELETE action, or name it in keylessColumns with why it holds no key"
	}

	return ""
}

// staleListings are the listed columns the schema no longer has, so neither list outlives what it describes.
func staleListings(seen map[string]bool) []string {
	var stale []string

	for name := range deliberateNonReferences {
		if !seen[name] {
			stale = append(stale, name)
		}
	}

	for name := range keylessColumns {
		if !seen[name] {
			stale = append(stale, name)
		}
	}

	return stale
}
