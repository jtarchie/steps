package resource

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...) //nolint:gosec // literals from the tests below
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}

	return strings.TrimSpace(string(out))
}

// A tag that points somewhere else than the version recorded fails the get by name, and checks out nothing.
func TestGitTagPatternGetFailsWhenTheTagMoved(t *testing.T) {
	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed")
	}

	origin := t.TempDir()
	gitRun(t, origin, "init", "-q", "-b", "main", ".")
	gitRun(t, origin, "commit", "-q", "--allow-empty", "-m", "one")
	gitRun(t, origin, "tag", "v1")
	checked := gitRun(t, origin, "rev-parse", "HEAD")
	gitRun(t, origin, "commit", "-q", "--allow-empty", "-m", "two")
	gitRun(t, origin, "tag", "-f", "v1")

	rt, err := config.ReadBuiltinResourceType("git")
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()

	// The shell's stderr goes to the process's, not into the error.
	logPath := filepath.Join(t.TempDir(), "stderr")

	logFile, err := os.Create(logPath) //nolint:gosec // under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}

	prev := os.Stderr
	os.Stderr = logFile

	defer func() { os.Stderr = prev }()

	err = RunIn(context.Background(), Call{
		Type:     rt,
		Resource: config.Resource{Source: map[string]any{"uri": origin, "tag_pattern": "v*"}},
		Version:  map[string]any{"tag": "v1", "ref": checked},
		Dir:      dest,
	})
	os.Stderr = prev

	logs, _ := os.ReadFile(logPath) //nolint:gosec // under t.TempDir()

	if err == nil || !strings.Contains(string(logs), "tag v1 moved since it was checked") {
		t.Fatalf("RunIn = %v, stderr %q, want the tag moved failure", err, logs)
	}
}
