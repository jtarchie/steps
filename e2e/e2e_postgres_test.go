package e2e

// --db postgres://… end to end: the CLI's scheme switch, the driver behind
// it, and what a person sees printed — against a real server started through
// internal/dockerapi whenever a daemon is reachable, never opt-in.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/postgres"
)

const postgresImage = "postgres:17-alpine"

// postgresServer is this shard process's one test server, started by the
// first test that asks and removed by TestMain.
var postgresServer struct {
	once     sync.Once
	id       string
	host     string
	password string
	skip     string
	err      error
	schemas  atomic.Int64
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

	t.Setenv("PGPASSWORD", postgresServer.password)

	schema := fmt.Sprintf("e2e_%d", postgresServer.schemas.Add(1))

	return (&url.URL{
		Scheme:   "postgres",
		User:     url.User("postgres"),
		Host:     postgresServer.host,
		Path:     "/postgres",
		RawQuery: "sslmode=disable&search_path=" + schema,
	}).String()
}

func startPostgresE2E() {
	ctx := context.Background()

	docker, err := dockerapi.New("")
	if err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = docker.Ping(pingCtx)

		cancel()
	}

	if err != nil {
		postgresServer.skip = fmt.Sprintf("no docker daemon to start postgres on: %v", err)

		if docker != nil {
			_ = docker.Close()
		}

		return
	}

	defer func() { _ = docker.Close() }()

	postgresServer.err = runPostgresE2E(ctx, docker)
}

func runPostgresE2E(ctx context.Context, docker *dockerapi.Client) error {
	// A shard killed outright never reached TestMain's removal.
	stale, err := docker.ListContainers(ctx, map[string]string{"steps.test": "postgres"})
	if err == nil {
		for _, container := range stale {
			pid, err := strconv.Atoi(container.Labels["steps.pid"])
			if err != nil || !processAlive(pid) {
				_ = docker.RemoveContainer(ctx, container.ID)
			}
		}
	}

	if !docker.ImagePresent(ctx, postgresImage) {
		err = docker.Pull(ctx, postgresImage, io.Discard)
		if err != nil {
			return fmt.Errorf("pulling %s: %w", postgresImage, err)
		}
	}

	postgresServer.password = rand.Text()

	// Loopback only, with a password nothing else knows.
	postgresServer.id, err = docker.CreateContainer(ctx, dockerapi.ContainerSpec{
		Image:   postgresImage,
		Cmd:     []string{"postgres", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"},
		Env:     []string{"POSTGRES_PASSWORD=" + postgresServer.password},
		Labels:  map[string]string{"steps.test": "postgres", "steps.pid": strconv.Itoa(os.Getpid())},
		Publish: []string{"5432/tcp"},
	})
	if err != nil {
		return fmt.Errorf("creating the test postgres: %w", err)
	}

	err = docker.StartContainer(ctx, postgresServer.id)
	if err != nil {
		return fmt.Errorf("starting the test postgres: %w", err)
	}

	port, err := docker.PublishedPort(ctx, postgresServer.id, "5432/tcp")
	if err != nil {
		return fmt.Errorf("starting the test postgres: %w", err)
	}

	postgresServer.host = "127.0.0.1:" + port

	return waitForPostgresE2E() //nolint:contextcheck // opening a database takes no context in either driver
}

// waitForPostgresE2E asks until the server answers "nothing recorded", which
// is the answer an up server gives about a schema nobody wrote.
func waitForPostgresE2E() error {
	probe := (&url.URL{
		Scheme: "postgres", User: url.UserPassword("postgres", postgresServer.password),
		Host: postgresServer.host, Path: "/postgres", RawQuery: "sslmode=disable&search_path=probe",
	}).String()

	deadline := time.Now().Add(60 * time.Second)

	for {
		reader, err := postgres.OpenReader(probe)
		if reader != nil {
			_ = reader.Close()
		}

		if errors.Is(err, store.ErrNoState) {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the test postgres never answered: %w", err)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

// stopPostgresE2E removes this shard's server, if it started one, under its
// own context and bound.
func stopPostgresE2E() {
	if postgresServer.id == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	docker, err := dockerapi.New("")
	if err != nil {
		return
	}

	defer func() { _ = docker.Close() }()

	_ = docker.RemoveContainer(ctx, postgresServer.id)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

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

// TestPostgresRunsAndCaches is the seam: from --db's scheme, through the
// CLI's switch, into the driver — and the cache behind it, which is the
// point of a state database. A second run of unchanged content is a skip.
func TestPostgresRunsAndCaches(t *testing.T) {
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

// TestPostgresWithNothingRecorded: a schema steps never wrote is "no runs
// yet", as a sqlite file that is not there is — and asking creates nothing.
func TestPostgresWithNothingRecorded(t *testing.T) {
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

// TestPostgresPasswordIsNeverPrinted: a password in --db is warned about,
// and appears in nothing steps prints — not the run's output, not a parked
// approval's pasteable command, not a log line.
func TestPostgresPasswordIsNeverPrinted(t *testing.T) {
	db := requirePostgresE2E(t)

	parsed, err := url.Parse(db)
	if err != nil {
		t.Fatal(err)
	}

	parsed.User = url.UserPassword("postgres", postgresServer.password)
	withPassword := parsed.String()

	t.Setenv("PGPASSWORD", "")

	stderr := captureStderr(t)
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "1s")

	out := captureStdout(t, func() {
		_ = cli.Run([]string{"run", path, "--job", "publish", "--db", withPassword})
	})

	if strings.Contains(out, postgresServer.password) || strings.Contains(stderr(), postgresServer.password) {
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

// TestPostgresABrokenPipelineCanBeDestroyed: destroying a pipeline nothing is serving opens the store itself, and it must open the one --db names — a sqlite file at a path spelled like the url would take the delete, and the pipeline would be broken again on the next restart.
func TestPostgresABrokenPipelineCanBeDestroyed(t *testing.T) {
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
