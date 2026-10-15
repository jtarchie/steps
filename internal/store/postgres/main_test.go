package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/goleak"

	"github.com/jtarchie/steps/internal/testpostgres"
)

var (
	// server is the running test database's superuser URL, on its
	// "postgres" database; empty when its binaries could not be had, which
	// skips every test here rather than failing them.
	server string
	// admin is a pool on server, for creating and dropping databases.
	admin *sql.DB
	// skipped says why server is empty.
	skipped string
)

// TestMain starts one Postgres for the package and removes it after, whenever its binaries can be had and never opt-in. It also enforces that nothing leaks a goroutine: every handle owns a database/sql pool, and a pool runs one until Close.
//
// Hand-rolled rather than goleak.VerifyTestMain, which checks for leaks before any cleanup it runs, and the admin pool is a goroutine until it is closed.
func TestMain(m *testing.M) {
	stop, err := startServer()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	code := m.Run()

	stop()

	if code == 0 {
		err = goleak.Find()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}

	os.Exit(code)
}

// startServer runs the package's one Postgres. Binaries that cannot be had are a skip; anything after that is a failure.
func startServer() (func(), error) {
	pg, err := testpostgres.Start(testpostgres.Fast)
	if errors.Is(err, testpostgres.ErrUnavailable) {
		skipped = err.Error()

		return func() {}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("starting the test postgres: %w", err)
	}

	server = pg.URL()

	admin, err = waitForServer(context.Background(), server)
	if err != nil {
		pg.Stop()

		return nil, err
	}

	return func() {
		_ = admin.Close()

		pg.Stop()
	}, nil
}

// waitForServer pings until the server answers, bounded.
func waitForServer(ctx context.Context, rawURL string) (*sql.DB, error) {
	config, _, err := connConfig(rawURL)
	if err != nil {
		return nil, err
	}

	db := stdlib.OpenDB(*config)
	deadline := time.Now().Add(60 * time.Second)

	for {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = db.PingContext(pingCtx)

		cancel()

		if err == nil {
			return db, nil
		}

		if time.Now().After(deadline) {
			_ = db.Close()

			return nil, fmt.Errorf("the test postgres never answered: %w", err)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

// requirePostgres skips a test when there is no server to run it against.
func requirePostgres(t *testing.T) {
	t.Helper()

	if server == "" {
		t.Skip(skipped)
	}
}

var databases atomic.Int64

// newDatabase creates an empty database for one test and drops it after,
// returning its URL. A database per test, not a schema: advisory locks and
// the shared node_content sweep are per database, and a test about them must
// not be serialized behind, or race with, a neighbour.
func newDatabase(t *testing.T) string {
	t.Helper()
	requirePostgres(t)

	name := fmt.Sprintf("t_%d", databases.Add(1))

	_, err := admin.ExecContext(t.Context(), `CREATE DATABASE `+quote(name))
	if err != nil {
		t.Fatalf("CREATE DATABASE %s: %v", name, err)
	}

	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.WithoutCancel(t.Context()), `DROP DATABASE `+quote(name)+` WITH (FORCE)`)
	})

	return withPath(t, server, "/"+name)
}

// withPath is rawURL pointing at another database.
func withPath(t *testing.T, rawURL, path string) string {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}

	parsed.Path = path

	return parsed.String()
}

// withQuery is rawURL with one more query parameter.
func withQuery(t *testing.T, rawURL, key, value string) string {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}

	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

// openStore opens a handle and closes it with the test.
func openStore(t *testing.T, rawURL, pipeline string) *Store {
	t.Helper()

	st, err := OpenStore(rawURL, pipeline)
	if err != nil {
		t.Fatalf("OpenStore(%q): %v", pipeline, err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}
