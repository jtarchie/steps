package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDockerSocketPathReadsTheSelectedContext: a worker reached over ssh has
// no shell profile and so no DOCKER_HOST, while `docker context use colima`
// has written where its daemon is. The shim has to answer the way docker
// would, or a machine whose `docker` works forwards a socket to nothing.
func TestDockerSocketPathReadsTheSelectedContext(t *testing.T) {
	root := t.TempDir()
	digest := sha256.Sum256([]byte("colima"))
	dir := filepath.Join(root, "contexts", "meta", hex.EncodeToString(digest[:]))

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	writeTestJSON(t, filepath.Join(dir, "meta.json"), map[string]any{
		"Name":      "colima",
		"Endpoints": map[string]any{"docker": map[string]any{"Host": "unix:///home/jt/.colima/default/docker.sock"}},
	})
	writeTestJSON(t, filepath.Join(root, "config.json"), map[string]any{"currentContext": "colima"})

	t.Setenv("DOCKER_CONFIG", root)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")

	if got := dockerSocketPath(""); got != "/home/jt/.colima/default/docker.sock" {
		t.Errorf("dockerSocketPath() = %q, want the selected context's socket", got)
	}

	// An explicit --docker-socket still wins, and DOCKER_HOST still beats
	// the store, as it does for docker itself.
	if got := dockerSocketPath("/custom.sock"); got != "/custom.sock" {
		t.Errorf("dockerSocketPath(configured) = %q, want the configured path", got)
	}

	t.Setenv("DOCKER_HOST", "unix:///env.sock")

	if got := dockerSocketPath(""); got != "/env.sock" {
		t.Errorf("dockerSocketPath() = %q, want DOCKER_HOST over the store", got)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, raw, 0o600)
	if err != nil {
		t.Fatal(err)
	}
}
