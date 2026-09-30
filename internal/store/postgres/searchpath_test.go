package postgres

import (
	"context"
	"testing"
)

// TestAnotherApplicationsTablesAreNeverAdopted: steps goes into a database
// somebody already runs, where public may well hold a table called runs. With
// public on the search_path, CREATE TABLE IF NOT EXISTS would see it and
// create nothing, and steps would write its runs into their table — or a
// planted schema_version would answer for steps' own.
func TestAnotherApplicationsTablesAreNeverAdopted(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	execAs(t, rawURL,
		`CREATE TABLE public.runs (id INTEGER, note TEXT)`,
		`INSERT INTO public.runs VALUES (1, 'theirs')`,
		`CREATE TABLE public.schema_version (version INTEGER)`,
		`INSERT INTO public.schema_version VALUES (999)`)

	st := openStore(t, rawURL, "p")

	err := st.StartRun(context.Background(), "r1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	db := rawDB(t, rawURL)

	if got := queryInt(t, db, `SELECT COUNT(*) FROM public.runs`); got != 1 {
		t.Errorf("public.runs holds %d rows, want its own one", got)
	}

	if got := queryInt(t, db, `SELECT COUNT(*) FROM steps.runs`); got != 1 {
		t.Errorf("steps.runs holds %d rows, want the run just started", got)
	}
}

// TestSearchPathChoosesTheSchema: two schemas in one database are two state
// databases, and their descriptions must say so — the web overview groups
// handles by description before asking one Reader about all of them.
func TestSearchPathChoosesTheSchema(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)

	first := openStore(t, rawURL, "p")
	second := openStore(t, withQuery(t, rawURL, "search_path", "other, public"), "p")

	if first.Description() == second.Description() {
		t.Fatalf("two schemas describe themselves as %q", first.Description())
	}

	err := second.StartRun(context.Background(), "r1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	db := rawDB(t, rawURL)

	if got := queryInt(t, db, `SELECT COUNT(*) FROM other.runs`); got != 1 {
		t.Errorf("other.runs holds %d rows, want 1", got)
	}

	if got := queryInt(t, db, `SELECT COUNT(*) FROM steps.runs`); got != 0 {
		t.Errorf("steps.runs holds %d rows, want none — the URL's schema was not the one written", got)
	}

	// The rest of the URL's path is dropped, not searched: public is where
	// another application's tables are.
	if got := queryInt(t, db, `SELECT COUNT(*) FROM pg_tables WHERE schemaname = 'public'`); got != 0 {
		t.Errorf("public holds %d tables, want none", got)
	}
}
