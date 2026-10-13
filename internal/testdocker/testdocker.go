// Package testdocker is a test's docker half of the setup: whether a daemon is reachable, and a directory it can bind-mount. A package rather than a _test.go file because seven packages carried their own copies.
package testdocker

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// unreachable is why there is no usable daemon, or "" when there is. Asked once per process: a test binary used to run `docker info` once per test, which a loaded VM answers in seconds. Asked through dockerapi, so a test finds the daemon the code under test would — by docker's context rules, with no docker binary on PATH.
var unreachable = sync.OnceValue(func() string { //nolint:gochecknoglobals // a per-process memo
	client, err := dockerapi.New("")
	if err != nil {
		return "no usable docker daemon: " + err.Error()
	}

	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.Ping(ctx)
	if err != nil {
		return err.Error()
	}

	return ""
})

// Require skips the calling test unless a usable daemon is reachable. Not opt-in: a test guarding a shipped feature does not get to be optional, and one that was is how a containerized placed step shipped not working. A test that BIND-MOUNTS needs MountableDir as well.
func Require(t testing.TB) {
	t.Helper()

	if reason := unreachable(); reason != "" {
		t.Skip(reason)
	}
}

// MountableDir is a temp directory the daemon can actually see. When it runs in a VM (Docker Desktop, colima) only some host paths are shared into it: the home directory is, macOS's $TMPDIR (/var/folders/...) is not — and an unshared bind mount is not an error, docker mounts an EMPTY directory, so a step succeeds and produces nothing. A real run is under the same constraint; see docs/infra.md on TMPDIR.
func MountableDir(t testing.TB) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to root a daemon-visible temp dir in")
	}

	dir, err := os.MkdirTemp(home, ".steps-test-*")
	if err != nil {
		t.Skipf("cannot create a daemon-visible temp dir under %s: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}
