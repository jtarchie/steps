// Package testdocker is a test's docker half of the setup: whether a daemon is reachable, and a tree in a volume. A package rather than a _test.go file because seven packages carried their own copies.
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
	reason := probe()
	if reason != "" {
		markUnreachable(reason)
	}

	return reason
})

// unreachableMarkerEnv names the file `task` asks a skipping process to write, so the commit stamp can say its docker tier never ran (tools/stamp).
const unreachableMarkerEnv = "STEPS_DOCKER_UNREACHABLE_MARKER"

// markUnreachable is best-effort: the marker only adds a line to the stamp's output, and a test binary has nowhere better to report a failed write.
func markUnreachable(reason string) {
	if path := os.Getenv(unreachableMarkerEnv); path != "" {
		_ = os.WriteFile(path, []byte(reason+"\n"), 0o600) //nolint:gosec // a path the Taskfile names for the test process it starts
	}
}

func probe() string {
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
}

// Require skips the calling test unless a usable daemon is reachable. Not opt-in: a test guarding a shipped feature does not get to be optional, and one that was is how a containerized placed step shipped not working. A test that needs a tree in its container seeds a volume with Tree.
func Require(t testing.TB) {
	t.Helper()

	if reason := unreachable(); reason != "" {
		t.Skip(reason)
	}
}
