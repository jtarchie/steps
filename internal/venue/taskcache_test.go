package venue

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestBareStepWithoutCachesSpendsNoRoundTrip: the zero session has no connection, so any exec here panics.
func TestBareStepWithoutCachesSpendsNoRoundTrip(t *testing.T) {
	t.Parallel()

	s := &bareSession{}

	err := s.attachCaches(WithLiveTaskCaches(t.Context(), "p", nil))
	if err != nil {
		t.Fatal(err)
	}
}

// TestBareCloseFilesBackOnlyAttachedCaches: until attachCaches has run, a cache path inside an input holds the input's upload, and filing that back would replace the worker's cache with it.
func TestBareCloseFilesBackOnlyAttachedCaches(t *testing.T) {
	t.Parallel()

	s := &bareSession{dir: "/w/steps-build.x", spec: shell.RunnerSpec{Caches: map[string]string{"src/node_modules": "abc"}}}

	if script := s.detachCaches(); script != "" {
		t.Errorf("close would file back a cache never attached: %q", script)
	}

	s.cachesAttached = true

	if s.detachCaches() == "" {
		t.Error("close would leave an attached cache behind")
	}
}

// TestBareDetachNeverNestsOnLinux: a plain mv onto a directory a concurrent close just filed moves the cache inside it; GNU mv -T refuses instead.
func TestBareDetachNeverNestsOnLinux(t *testing.T) {
	t.Parallel()

	s := &bareSession{dir: "/w/steps-build.x", cachesAttached: true, spec: shell.RunnerSpec{Caches: map[string]string{"c": "abc"}}}

	s.goos = "linux"
	if script := s.detachCaches(); !strings.Contains(script, "mv -T ") || strings.Contains(script, "; mv '") {
		t.Errorf("a linux worker files the cache back without mv -T: %q", script)
	}

	s.goos = "darwin"
	if script := s.detachCaches(); strings.Contains(script, "mv -T") {
		t.Errorf("a worker without GNU mv is handed -T: %q", script)
	}
}

// TestBareCachesNeverFollowASymlinkOutOfTheStep: an uploaded input keeps its symlinks, so a src linked elsewhere must not have that elsewhere's node_modules removed, nor filed into the cache; a path with no link still round-trips.
func TestBareCachesNeverFollowASymlinkOutOfTheStep(t *testing.T) {
	t.Parallel()

	root, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "steps-build.x")

	err := os.MkdirAll(filepath.Join(outside, "node_modules"), 0o750)
	if err == nil {
		err = os.WriteFile(filepath.Join(outside, "node_modules/precious"), []byte("x"), 0o600)
	}

	if err == nil {
		err = os.MkdirAll(dir, 0o750)
	}

	if err == nil {
		err = os.Symlink(outside, filepath.Join(dir, "src"))
	}

	if err != nil {
		t.Fatal(err)
	}

	s := &bareSession{dir: dir, worker: Worker{Root: root}, cachesAttached: true, spec: shell.RunnerSpec{Caches: map[string]string{"src/node_modules": "abc", "c/d": "def"}}}

	out, err := exec.CommandContext(t.Context(), "sh", "-c", s.attachScript()+s.detachCaches()).CombinedOutput() //nolint:gosec // the scripts under test, over t.TempDir() paths
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	_, err = os.Stat(filepath.Join(outside, "node_modules/precious"))
	if err != nil {
		t.Errorf("a cache reached through a symlink out of the step: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, "steps-task-caches/def"))
	if err != nil || !info.IsDir() {
		t.Errorf("a cache with no link above it was not filed back: %v", err)
	}
}
