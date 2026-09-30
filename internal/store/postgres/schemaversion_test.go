package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jtarchie/steps/internal/store"
)

// execAs runs statements on rawURL's database outside any steps handle —
// what an operator, or another application sharing the database, does.
func execAs(t *testing.T, rawURL string, statements ...string) {
	t.Helper()

	db := rawDB(t, rawURL)

	for _, statement := range statements {
		_, err := db.ExecContext(t.Context(), statement)
		if err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// rawDB is a pool on rawURL with the server's own search_path, closed with
// the test.
func rawDB(t *testing.T, rawURL string) *sql.DB {
	t.Helper()

	config, _, err := connConfig(rawURL)
	if err != nil {
		t.Fatal(err)
	}

	delete(config.RuntimeParams, "search_path")

	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func queryInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()

	var value int

	err := db.QueryRowContext(t.Context(), query).Scan(&value)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}

	return value
}

func TestAFreshDatabaseIsStamped(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	if got := queryInt(t, rawDB(t, rawURL), `SELECT version FROM steps.schema_version`); got != schemaVersion {
		t.Errorf("schema_version = %d, want %d", got, schemaVersion)
	}
}

// TestAMismatchedSchemaIsRefusedAndLeftAlone: refused rather than migrated,
// with advice that loses only steps' data — and the refused open adds nothing
// to a schema some other build wrote.
func TestAMismatchedSchemaIsRefusedAndLeftAlone(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	execAs(t, rawURL,
		`CREATE SCHEMA steps`,
		`CREATE TABLE steps.schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO steps.schema_version VALUES (999)`)

	_, err := OpenStore(rawURL, "p")
	if !errors.Is(err, store.ErrSchemaVersion) {
		t.Fatalf("OpenStore = %v, want ErrSchemaVersion", err)
	}

	if !strings.Contains(err.Error(), `DROP SCHEMA "steps" CASCADE`) {
		t.Errorf("the refusal %q does not say how to recover", err)
	}

	tables := queryInt(t, rawDB(t, rawURL), `SELECT COUNT(*) FROM pg_tables WHERE schemaname = 'steps'`)
	if tables != 1 {
		t.Errorf("the refused schema holds %d tables, want only its schema_version", tables)
	}
}

// TestConcurrentFirstOpensAllSucceed: two daemons' first starts, or a daemon
// and a `steps run`, each see no schema and each run the DDL — the loser
// failing on a duplicate unless the creation is serialized.
func TestConcurrentFirstOpensAllSucceed(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Go(func() {
			st, err := OpenStore(rawURL, fmt.Sprintf("p%d", i))
			if err != nil {
				t.Errorf("OpenStore %d: %v", i, err)

				return
			}

			_ = st.Close()
		})
	}

	wg.Wait()
}

// TestAMatchingSchemaRunsNoDDL: once the schema is there, a role that may use
// the tables but create nothing opens it. Running the DDL anyway would need
// CREATE on the schema, and cost sixty statements over the network per open.
func TestAMatchingSchemaRunsNoDDL(t *testing.T) {
	t.Parallel()

	role := newRole(t)
	rawURL := newDatabase(t)
	openStore(t, rawURL, "p")

	execAs(t, rawURL,
		`GRANT USAGE ON SCHEMA steps TO `+role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA steps TO `+role,
		`GRANT USAGE ON ALL SEQUENCES IN SCHEMA steps TO `+role)

	st, err := OpenStore(asRole(t, rawURL, role, "pw"), "another")
	if err != nil {
		t.Fatalf("OpenStore as a role without CREATE: %v", err)
	}

	defer func() { _ = st.Close() }()

	err = st.StartRun(context.Background(), "r1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun as that role: %v", err)
	}
}

func mustURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}

	return parsed
}

func asRole(t *testing.T, rawURL, role, password string) string {
	t.Helper()

	parsed := mustURL(t, rawURL)
	parsed.User = url.UserPassword(role, password)

	return parsed.String()
}

// newRole creates a login role with password "pw". Call it before
// newDatabase: cleanups run last first, and the role can only be dropped once
// the database holding its grants is gone.
func newRole(t *testing.T) string {
	t.Helper()
	requirePostgres(t)

	role := fmt.Sprintf("role_%d", databases.Add(1))

	_, err := admin.ExecContext(t.Context(), `CREATE ROLE `+role+` LOGIN PASSWORD 'pw'`)
	if err != nil {
		t.Fatalf("CREATE ROLE: %v", err)
	}

	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.WithoutCancel(t.Context()), `DROP ROLE IF EXISTS `+role)
	})

	return role
}
