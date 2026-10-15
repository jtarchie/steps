// Package testpostgres runs a real Postgres for tests from embedded-postgres' downloaded binaries, so the Postgres-backed tests need no docker daemon.
package testpostgres

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// ErrUnavailable means the server's binaries could not be had (the first run on a machine downloads them), which a caller treats as a skip. Anything else Start returns is a failure.
var ErrUnavailable = errors.New("postgres binaries unavailable")

// Fast trades durability for speed, for tests whose database dies with the run. A measurement leaves it out: Postgres' own defaults are what an operator gets.
var Fast = map[string]string{
	"max_connections":    "500",
	"fsync":              "off",
	"synchronous_commit": "off",
	"full_page_writes":   "off",
}

// Server is a running Postgres reachable on loopback only, with a password nothing else knows.
type Server struct {
	// Host is "127.0.0.1:port".
	Host     string
	Password string

	db   *embeddedpostgres.EmbeddedPostgres
	root string
}

// URL is the superuser's URL on the "postgres" database, password included.
func (s *Server) URL() string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", s.Password),
		Host:     s.Host,
		Path:     "/postgres",
		RawQuery: "sslmode=disable",
	}).String()
}

// workRoot holds one directory per test process, named by pid so a later run can tell a dead process's server from a live one's.
func workRoot() string { return filepath.Join(os.TempDir(), "steps-testpg") }

// Start sweeps first because a run killed outright never reached its own Stop.
func Start(settings map[string]string) (*Server, error) {
	sweep()

	listener, err := (&net.ListenConfig{}).Listen(nil, "tcp", "127.0.0.1:0") //nolint:staticcheck // no context to give a test fixture's port probe
	if err != nil {
		return nil, fmt.Errorf("finding a port for the test postgres: %w", err)
	}

	host := listener.Addr().String()
	_ = listener.Close()

	_, portText, _ := net.SplitHostPort(host)

	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("port %q: %w", portText, err)
	}

	root := filepath.Join(workRoot(), strconv.Itoa(os.Getpid()))
	password := rand.Text()

	db := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).
		Port(uint32(port)).
		Password(password).
		RuntimePath(filepath.Join(root, "run")).
		DataPath(filepath.Join(root, "data")).
		StartParameters(settings).
		StartTimeout(3 * time.Minute).
		Logger(io.Discard))

	err = db.Start()
	if err != nil {
		_ = os.RemoveAll(root)

		if strings.Contains(err.Error(), "unable to connect to") || strings.Contains(err.Error(), "error fetching postgres") {
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}

		return nil, fmt.Errorf("starting the test postgres: %w", err)
	}

	return &Server{Host: host, Password: password, db: db, root: root}, nil
}

// Stop is nil-safe so a deferred Stop works after a start that was skipped.
func (s *Server) Stop() {
	if s == nil {
		return
	}

	_ = s.db.Stop()
	_ = os.RemoveAll(s.root)
}

// sweep stops servers whose test process is gone: a run killed outright never reached its own Stop. A live pid's server is another test process's, running now, and is left alone.
func sweep() {
	entries, err := os.ReadDir(workRoot())
	if err != nil {
		return
	}

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || alive(pid) {
			continue
		}

		dir := filepath.Join(workRoot(), entry.Name())
		stopPostmaster(filepath.Join(dir, "data", "postmaster.pid"))
		_ = os.RemoveAll(dir)
	}
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

// stopPostmaster signals the server a dead run left, but only a process that is still a postgres: its pid may since belong to something else.
func stopPostmaster(pidFile string) {
	body, err := os.ReadFile(pidFile) //nolint:gosec // a path under this fixture's own work directory
	if err != nil {
		return
	}

	first, _, _ := strings.Cut(string(body), "\n")

	pid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 1 {
		return
	}

	name, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec,noctx // pid parsed above; a one-shot probe
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(name)), "postgres") {
		return
	}

	_ = syscall.Kill(pid, syscall.SIGTERM)
}
