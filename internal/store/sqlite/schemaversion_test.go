package sqlite

// Opening a database this build cannot write to.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestOpenStoreRefusesAnOlderSchema is the alternative to losing a run's whole
// record in silence.
//
// The schema is CREATE TABLE IF NOT EXISTS and nothing rewrites an older
// database — deliberately, pre-release. But "nothing rewrites it" was being
// read as "nothing notices it": a database missing a column simply failed
// every INSERT that named it, and AppendRunEvent only warns, so the build went
// GREEN with an empty record and one WRN line to explain it. Whoever ran it
// learns their history is gone when they open the web UI.
//
// Refusing at open is what turns that into an instruction.
func TestOpenStoreRefusesAnOlderSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	fresh, err := OpenStore(path, "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	// With history in it. An EMPTY database at an older version is
	// indistinguishable from a new one and has nothing to lose, so it is
	// stamped rather than refused — the refusal is for the file somebody
	// would be losing something by deleting.
	err = fresh.StartRun(context.Background(), "R1", "build", t.TempDir(), "")
	if err != nil {
		t.Fatalf("recording a run: %v", err)
	}

	err = fresh.Close()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}

	stampVersion(t, path, schemaVersion-1)

	_, err = OpenStore(path, "test")
	if err == nil {
		t.Fatal("a database written by an older build was opened — its records are silently discarded from here on")
	}

	if !errors.Is(err, store.ErrSchemaVersion) {
		t.Fatalf("error = %v, want a schema-version refusal", err)
	}

	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the file to delete", err)
	}
}

// TestOpenStoreAdoptsAnEmptyOlderDatabase pins the other side of that: a
// leftover file with no runs and no nodes is not worth an error message. The
// DDL just created every table at its current definition, so it is a new
// database in every way that matters.
func TestOpenStoreAdoptsAnEmptyOlderDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	st, err := OpenStore(path, "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	stampVersion(t, path, 0)

	st, err = OpenStore(path, "test")
	if err != nil {
		t.Fatalf("an empty database was refused: %v", err)
	}

	_ = st.Close()
}

// TestOpenStoreAcceptsItsOwnDatabase pins the ordinary path: a database this
// build created reopens, repeatedly.
func TestOpenStoreAcceptsItsOwnDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	for range 3 {
		st, err := OpenStore(path, "test")
		if err != nil {
			t.Fatalf("OpenStore: %v", err)
		}

		err = st.Close()
		if err != nil {
			t.Fatalf("closing: %v", err)
		}
	}
}

// TestOpenStoreRefusesANewerSchema covers the other direction: an older steps
// run against a database a newer one wrote would find its columns but not its
// meaning.
func TestOpenStoreRefusesANewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	st, err := OpenStore(path, "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	_ = st.Close()

	stampVersion(t, path, schemaVersion+1)

	_, err = OpenStore(path, "test")
	if !errors.Is(err, store.ErrSchemaVersion) {
		t.Fatalf("error = %v, want a schema-version refusal", err)
	}
}

func stampVersion(t *testing.T, path string, version int) {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening %q directly: %v", path, err)
	}

	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(context.Background(), "PRAGMA user_version = "+strconv.Itoa(version))
	if err != nil {
		t.Fatalf("stamping user_version: %v", err)
	}
}

// TestSchemaAndStampLandInOneCommit: the reader tells a file mid-creation
// from an old one by whether the schema is THERE, so a writer that commits
// its tables before its stamp shows a reader tables at version 0 — and the
// reader's answer to that is "written by a different build, delete the file",
// about the database the operator just started. Racing `steps runs` against
// the first `steps run` is all it takes.
//
// So the DDL and the stamp are one transaction, and this holds the
// transaction open to look from a second connection: nothing, then both.
func TestSchemaAndStampLandInOneCommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	writer, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}

	defer func() { _ = writer.Close() }()

	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	err = initSchema(ctx, tx, path)
	if err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	reader, err := openReadOnlyDB(path)
	if err != nil {
		t.Fatalf("openReadOnlyDB: %v", err)
	}

	defer func() { _ = reader.Close() }()

	missing, found := readerSees(ctx, t, reader, path)
	if !missing || found != 0 {
		t.Fatalf("before commit a reader sees schema missing=%v at version %d, want missing at 0 — it will call this file an older build's and tell the operator to delete it", missing, found)
	}

	err = tx.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	missing, found = readerSees(ctx, t, reader, path)
	if missing || found != schemaVersion {
		t.Fatalf("after commit a reader sees schema missing=%v at version %d, want present at %d", missing, found, schemaVersion)
	}
}

// readerSees is what OpenReader would decide from: whether the schema is
// there, and what version the file claims.
func readerSees(ctx context.Context, t *testing.T, db *sql.DB, path string) (missing bool, version int) {
	t.Helper()

	missing, err := schemaMissing(ctx, db)
	if err != nil {
		t.Fatalf("schemaMissing: %v", err)
	}

	version, err = readSchemaVersion(ctx, db, path)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}

	return missing, version
}
