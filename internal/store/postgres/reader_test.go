package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jtarchie/steps/internal/store"
)

// TestAnEmptyDatabaseHasNothingRecorded: a database steps never wrote is "no
// runs yet", the answer sqlite gives a file with no tables — and asking
// leaves it as empty as it was.
func TestAnEmptyDatabaseHasNothingRecorded(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)

	_, err := OpenReader(rawURL)
	if !errors.Is(err, store.ErrNoState) {
		t.Fatalf("OpenReader = %v, want ErrNoState", err)
	}

	if !HasNothingRecorded(rawURL) {
		t.Error("HasNothingRecorded = false for a database steps never wrote")
	}

	_, err = OpenExisting(rawURL, "p")
	if !errors.Is(err, store.ErrNoState) {
		t.Errorf("OpenExisting = %v, want ErrNoState", err)
	}

	if got := queryInt(t, rawDB(t, rawURL), `SELECT COUNT(*) FROM pg_namespace WHERE nspname = 'steps'`); got != 0 {
		t.Error("a read created the schema")
	}
}

// TestAMissingDatabaseIsNotNothingRecorded: a typo in --db, or a database
// nobody created, must not read as "no runs yet" — and the error says what to
// do about it.
func TestAMissingDatabaseIsNotNothingRecorded(t *testing.T) {
	t.Parallel()
	requirePostgres(t)

	missing := withPath(t, server, "/no_such_database")

	_, err := OpenReader(missing)
	if err == nil || errors.Is(err, store.ErrNoState) {
		t.Fatalf("OpenReader = %v, want an error that is not ErrNoState", err)
	}

	if !strings.Contains(err.Error(), "createdb") {
		t.Errorf("the error %q does not say the database must be created", err)
	}

	if HasNothingRecorded(missing) {
		t.Error("HasNothingRecorded = true for a database that does not exist")
	}

	_, err = OpenStore(missing, "p")
	if err == nil || !strings.Contains(err.Error(), "createdb") {
		t.Errorf("OpenStore = %v, want the createdb advice", err)
	}
}

// TestAReaderNeverWrites: the sqlite driver can only promise it; a read-only
// session holds it to that, whatever a bug in this package tried.
func TestAReaderNeverWrites(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	reader, err := OpenReader(rawURL)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	defer func() { _ = reader.Close() }()

	_, err = reader.db.ExecContext(context.Background(), `INSERT INTO pipelines (name, path) VALUES ('sneaky', '')`)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Errorf("a write through a Reader = %v, want read_only_sql_transaction", err)
	}
}

// TestAReaderNeedsOnlySelect: `steps runs` against a database whose operator
// granted a reporting role nothing but SELECT.
func TestAReaderNeedsOnlySelect(t *testing.T) {
	t.Parallel()

	role := newRole(t)
	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	execAs(t, rawURL,
		`GRANT USAGE ON SCHEMA steps TO `+role,
		`GRANT SELECT ON ALL TABLES IN SCHEMA steps TO `+role)

	reader, err := OpenReader(asRole(t, rawURL, role, "pw"))
	if err != nil {
		t.Fatalf("OpenReader as a SELECT-only role: %v", err)
	}

	defer func() { _ = reader.Close() }()

	rows, err := reader.Pipelines(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("Pipelines = %v, %v; want p", rows, err)
	}
}

// TestOpenExistingNamesWhatTheDatabaseHolds, and writes: approving,
// answering and unpausing all resolve their pipeline this way.
func TestOpenExistingNamesWhatTheDatabaseHolds(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "infra")
	openStore(t, rawURL, "web")

	_, err := OpenExisting(rawURL, "wbe")
	if !errors.Is(err, store.ErrNoSuchPipeline) {
		t.Fatalf("OpenExisting = %v, want ErrNoSuchPipeline", err)
	}

	if !strings.Contains(err.Error(), "infra, web") {
		t.Errorf("the refusal %q does not list the pipelines held", err)
	}

	st, err := OpenExisting(rawURL, "web")
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}

	defer func() { _ = st.Close() }()

	err = st.Pause(context.Background())
	if err != nil {
		t.Errorf("Pause through OpenExisting: %v", err)
	}

	if got := queryInt(t, rawDB(t, rawURL), `SELECT COUNT(*) FROM steps.pipelines`); got != 2 {
		t.Errorf("%d pipelines after resolving one, want 2 — a lookup registered its subject", got)
	}
}
