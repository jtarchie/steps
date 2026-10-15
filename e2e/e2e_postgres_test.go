package e2e

// --db postgres://… end to end: the CLI's scheme switch, the driver behind
// it, and what a person sees printed — against a real server started from
// embedded-postgres' binaries whenever they can be had, never opt-in.

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/postgres"
	"github.com/jtarchie/steps/internal/testpostgres"
)

// postgresServer is this shard process's one test server, started by the first test that asks and removed by TestMain.
var postgresServer struct {
	once    sync.Once
	server  *testpostgres.Server
	skip    string
	err     error
	schemas atomic.Int64
}

// requirePostgresE2E returns a URL — with no password in it — on an empty
// schema of its own, and sets PGPASSWORD for the test, which is the way the
// docs say to pass one. Not parallel-safe, like every test here that swaps
// os.Stdout.
func requirePostgresE2E(t *testing.T) string {
	t.Helper()

	postgresServer.once.Do(startPostgresE2E)

	switch {
	case postgresServer.skip != "":
		t.Skip(postgresServer.skip)
	case postgresServer.err != nil:
		t.Fatal(postgresServer.err)
	}

	t.Setenv("PGPASSWORD", postgresServer.server.Password)

	schema := fmt.Sprintf("e2e_%d", postgresServer.schemas.Add(1))

	return (&url.URL{
		Scheme:   "postgres",
		User:     url.User("postgres"),
		Host:     postgresServer.server.Host,
		Path:     "/postgres",
		RawQuery: "sslmode=disable&search_path=" + schema,
	}).String()
}

func startPostgresE2E() {
	server, err := testpostgres.Start(testpostgres.Fast)

	switch {
	case errors.Is(err, testpostgres.ErrUnavailable):
		postgresServer.skip = err.Error()
	case err != nil:
		postgresServer.err = err
	default:
		postgresServer.server = server
	}
}

// stopPostgresE2E removes this shard's server, if it started one.
func stopPostgresE2E() { postgresServer.server.Stop() }

func postgresPipeline(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	return writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo compiled >> %s
`, log)), log
}

// TestPostgres is one top-level test so that one shard, and one server, runs it all: as four, the shard balancer dealt them apart and every shard started a postgres of its own.
func TestPostgres(t *testing.T) {
	t.Run("RunsAndCaches", postgresRunsAndCaches)
	t.Run("WithNothingRecorded", postgresWithNothingRecorded)
	t.Run("PasswordIsNeverPrinted", postgresPasswordIsNeverPrinted)
	t.Run("ABrokenPipelineCanBeDestroyed", postgresABrokenPipelineCanBeDestroyed)
}

// postgresRunsAndCaches is the seam: from --db's scheme, through the
// CLI's switch, into the driver — and the cache behind it, which is the
// point of a state database. A second run of unchanged content is a skip.
func postgresRunsAndCaches(t *testing.T) {
	db := requirePostgresE2E(t)
	path, log := postgresPipeline(t)

	for range 2 {
		var err error

		out := captureStdout(t, func() { err = cli.Run([]string{"run", path, "--job", "build", "--db", db}) })
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}
	}

	assertLineCount(t, log, 1)

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"runs", "list", "-p", cli.PipelineName(path), "--db", db})
	})
	if err != nil {
		t.Fatalf("runs list: %v", err)
	}

	if strings.Count(out, "build") < 2 {
		t.Errorf("runs list printed:\n%s\nwant both runs of build", out)
	}

	// The pasteable hint is the url as typed: one rebuilt from the resolved
	// config dropped sslmode, so pasting it fell back to libpq's prefer.
	if want := "--db " + shellQuoted(db); !strings.Contains(out, want) {
		t.Errorf("runs list's hint does not carry %s:\n%s", want, out)
	}
}

// postgresWithNothingRecorded: a schema steps never wrote is "no runs
// yet", as a sqlite file that is not there is — and asking creates nothing.
func postgresWithNothingRecorded(t *testing.T) {
	db := requirePostgresE2E(t)

	var err error

	out := captureStdout(t, func() { err = cli.Run([]string{"runs", "list", "-p", "app", "--db", db}) })
	if err != nil {
		t.Fatalf("runs list: %v", err)
	}

	if !strings.Contains(out, "no runs recorded yet") {
		t.Errorf("runs list printed %q, want no runs recorded yet", out)
	}

	if !postgres.HasNothingRecorded(db) {
		t.Error("asking about history created the schema it asked about")
	}
}

// postgresPasswordIsNeverPrinted: a password in --db is warned about,
// and appears in nothing steps prints — not the run's output, not a parked
// approval's pasteable command, not a log line.
func postgresPasswordIsNeverPrinted(t *testing.T) {
	db := requirePostgresE2E(t)

	parsed, err := url.Parse(db)
	if err != nil {
		t.Fatal(err)
	}

	parsed.User = url.UserPassword("postgres", postgresServer.server.Password)
	withPassword := parsed.String()

	t.Setenv("PGPASSWORD", "")

	stderr := captureStderr(t)
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "1s")

	out := captureStdout(t, func() {
		_ = cli.Run([]string{"run", path, "--job", "publish", "--db", withPassword})
	})

	if strings.Contains(out, postgresServer.server.Password) || strings.Contains(stderr(), postgresServer.server.Password) {
		t.Fatalf("the password was printed:\nstdout:\n%s\nstderr:\n%s", out, stderr())
	}

	if !strings.Contains(stderr(), "--db carries a password") {
		t.Errorf("no warning about the password in --db:\n%s", stderr())
	}

	// The approval's hint is printed to be pasted: quoted, because the url
	// carries ? and &, and without the password, which PGPASSWORD supplies.
	want := "--db " + shellQuoted(postgres.Redact(withPassword))
	if !strings.Contains(out, want) {
		t.Errorf("the parked approval's command does not carry %s:\n%s", want, out)
	}
}

func shellQuoted(value string) string { return "'" + value + "'" }

// postgresABrokenPipelineCanBeDestroyed: destroying a pipeline nothing is serving opens the store itself, and it must open the one --db names — a sqlite file at a path spelled like the url would take the delete, and the pipeline would be broken again on the next restart.
func postgresABrokenPipelineCanBeDestroyed(t *testing.T) {
	db := requirePostgresE2E(t)

	restarted, _, _ := restartWithABrokenPipelineOn(t, db)
	defer restarted.stopIfRunning(t)

	if restarted.onrootBrokenReason(t) == "" {
		t.Fatal("onroot is not held broken, so this proves nothing")
	}

	restarted.pipeline(t, "destroy", "-p", "onroot", "-n")
	restarted.stop(t)

	again := startWeb(t, "--db", db, "--interval", "1h")
	defer again.stopIfRunning(t)

	if reason := again.onrootBrokenReason(t); reason != "" {
		t.Errorf("onroot is broken again after a destroy and a restart, so the destroy missed the database: %s", reason)
	}

	again.stop(t)
}
