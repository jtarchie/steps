package e2e

// The loadtest's Postgres option: STEPS_LOADTEST_DB=postgres runs the same ramp against a server with Postgres' own durability defaults, since the Fast settings the tests use would flatter these numbers.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/postgres"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/testpostgres"
)

// loadtestPostgres starts a server for this test and returns a URL builder: each call names a fresh schema, so a level starts from an empty state.
func loadtestPostgres(t *testing.T) func(schema string) string {
	t.Helper()

	server, err := testpostgres.Start(nil)
	if err != nil {
		t.Fatalf("test postgres: %v", err)
	}

	t.Cleanup(server.Stop)

	return func(schema string) string {
		return (&url.URL{
			Scheme: "postgres", User: url.UserPassword("postgres", server.Password),
			Host: server.Host, Path: "/postgres", RawQuery: "sslmode=disable&search_path=" + schema,
		}).String()
	}
}

// loadtestOpen opens a pipeline's store from whichever kind of state the level runs on.
func loadtestOpen(state, pipeline string) (store.Store, error) {
	if strings.HasPrefix(state, "postgres://") {
		return postgres.OpenStore(state, pipeline) //nolint:wrapcheck // a test helper passing the error through
	}

	return sqlite.OpenStore(state, pipeline) //nolint:wrapcheck // a test helper passing the error through
}
