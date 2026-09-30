package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/goleak"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// testImage is the server every test here runs against.
const testImage = "postgres:17-alpine"

var (
	// server is the running test database's superuser URL, on its
	// "postgres" database; empty when there is no docker daemon to start one
	// on, which skips every test here rather than failing them.
	server string
	// admin is a pool on server, for creating and dropping databases.
	admin *sql.DB
	// skipped says why server is empty.
	skipped string
)

// TestMain starts one Postgres for the package, through internal/dockerapi,
// and removes it after — whenever a daemon is reachable, never opt-in, as
// with every docker-backed test here. It also enforces that nothing leaks a
// goroutine: every handle owns a database/sql pool, and a pool runs one until
// Close.
//
// Hand-rolled rather than goleak.VerifyTestMain, which checks for leaks
// before any cleanup it runs — and the admin pool and the docker client are
// both goroutines until they are closed.
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

// startServer runs the container and waits for it to answer. A daemon that
// cannot be reached is a skip; anything after that is a failure.
func startServer() (func(), error) {
	ctx := context.Background()

	docker, err := dockerapi.New("")
	if err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = docker.Ping(pingCtx)

		cancel()

		if err != nil {
			_ = docker.Close()
		}
	}

	if err != nil {
		skipped = fmt.Sprintf("no docker daemon to start postgres on: %v", err)

		return func() {}, nil
	}

	defer func() { _ = docker.Close() }()

	id, port, password, err := runContainer(ctx, docker)
	if err != nil {
		return nil, err
	}

	stop := func() {
		if admin != nil {
			_ = admin.Close()
		}

		removeContainer(id)
	}

	server = (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", password),
		Host:     "127.0.0.1:" + port,
		Path:     "/postgres",
		RawQuery: "sslmode=disable",
	}).String()

	admin, err = waitForServer(ctx, server)
	if err != nil {
		stop()

		return nil, err
	}

	return stop, nil
}

// runContainer starts the server published on loopback only, with a
// password nobody else knows: a test database reachable from another host
// with a guessable superuser is not something a test run gets to open.
//
// The -c flags trade durability for speed — nothing here outlives the run.
func runContainer(ctx context.Context, docker *dockerapi.Client) (string, string, string, error) {
	sweepStaleContainers(ctx, docker)

	if !docker.ImagePresent(ctx, testImage) {
		err := docker.Pull(ctx, testImage, io.Discard)
		if err != nil {
			return "", "", "", fmt.Errorf("pulling %s: %w", testImage, err)
		}
	}

	password := rand.Text()

	id, err := docker.CreateContainer(ctx, dockerapi.ContainerSpec{
		Image: testImage,
		Cmd: []string{
			"postgres", "-c", "max_connections=500", "-c", "fsync=off",
			"-c", "synchronous_commit=off", "-c", "full_page_writes=off",
		},
		Env:     []string{"POSTGRES_PASSWORD=" + password},
		Labels:  map[string]string{"steps.test": "postgres", "steps.pid": strconv.Itoa(os.Getpid())},
		Publish: []string{"5432/tcp"},
	})
	if err != nil {
		return "", "", "", fmt.Errorf("creating the test postgres: %w", err)
	}

	err = docker.StartContainer(ctx, id)
	if err == nil {
		var port string

		port, err = docker.PublishedPort(ctx, id, "5432/tcp")
		if err == nil {
			return id, port, password, nil
		}
	}

	removeContainer(id) //nolint:contextcheck // cleanup runs on its own context, since the caller's may be why it runs

	return "", "", "", fmt.Errorf("starting the test postgres: %w", err)
}

// sweepStaleContainers removes test servers whose process is gone — a run
// killed outright never reached its own cleanup. A live pid's container is
// another test process's, running now, and is left alone.
func sweepStaleContainers(ctx context.Context, docker *dockerapi.Client) {
	containers, err := docker.ListContainers(ctx, map[string]string{"steps.test": "postgres"})
	if err != nil {
		return
	}

	for _, container := range containers {
		pid, err := strconv.Atoi(container.Labels["steps.pid"])
		if err == nil && alive(pid) {
			continue
		}

		_ = docker.RemoveContainer(ctx, container.ID)
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

// removeContainer runs under its own context and bound, because the likeliest
// reason it runs early is that something already went wrong.
func removeContainer(id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
	defer cancel()

	docker, err := dockerapi.New("")
	if err != nil {
		return
	}

	defer func() { _ = docker.Close() }()

	_ = docker.RemoveContainer(ctx, id)
}

// waitForServer pings until the server answers, bounded: the image's first
// start initializes a cluster and restarts before it listens on TCP.
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
