package testdocker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The marker is the seam to tools/stamp: a process whose docker tests skip for want of a daemon must leave it, or the stamp vouches for a docker tier that never ran without a word. A child process, because the probe is memoized once per process.
func TestASkippingProcessLeavesTheMarker(t *testing.T) {
	if os.Getenv("TESTDOCKER_CHILD") == "1" {
		Require(t)

		return
	}

	t.Parallel()

	marker := filepath.Join(t.TempDir(), "marker")

	//nolint:gosec // this test binary, re-run on one of its own tests
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestASkippingProcessLeavesTheMarker$", "-test.v")
	child.Env = append(os.Environ(), "TESTDOCKER_CHILD=1", "DOCKER_HOST=unix:///nonexistent.sock", unreachableMarkerEnv+"="+marker)

	out, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- SKIP") {
		t.Fatalf("the child should skip for want of a daemon: %v\n%s", err, out)
	}

	reason, err := os.ReadFile(marker) //nolint:gosec // a path under this test's own temp dir
	if err != nil || strings.TrimSpace(string(reason)) == "" {
		t.Fatalf("a process that skipped its docker tests left no marker saying why: %q, %v", reason, err)
	}
}
