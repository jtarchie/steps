package dockerapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeContextStore builds a docker configuration directory and points
// DOCKER_CONFIG at it. current names the context config.json selects (empty
// writes no config.json at all); contexts maps a context name to its endpoint.
func writeContextStore(t *testing.T, current string, contexts map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, host := range contexts {
		digest := sha256.Sum256([]byte(name))
		dir := filepath.Join(root, "contexts", "meta", hex.EncodeToString(digest[:]))

		err := os.MkdirAll(dir, 0o700)
		if err != nil {
			t.Fatalf("making the context directory: %v", err)
		}

		writeJSON(t, filepath.Join(dir, "meta.json"), map[string]any{
			"Name":      name,
			"Endpoints": map[string]any{"docker": map[string]any{"Host": host}},
		})
	}

	if current != "" {
		writeJSON(t, filepath.Join(root, "config.json"), map[string]any{"currentContext": current})
	}

	t.Setenv("DOCKER_CONFIG", root)

	return root
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}

	err = os.WriteFile(path, encoded, 0o600)
	if err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// clearDockerEnv removes the variables that would otherwise decide the answer,
// so a test about the context store is not quietly answered by the
// environment the suite happens to run in.
func clearDockerEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		t.Setenv(name, "")

		err := os.Unsetenv(name)
		if err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
}

func TestResolveHostPrefersDockerHost(t *testing.T) {
	clearDockerEnv(t)
	writeContextStore(t, "ctx", map[string]string{"ctx": "unix:///from/the/context.sock"})
	t.Setenv("DOCKER_HOST", "tcp://from-the-env:2375")

	got, err := ResolveHost()
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}

	if got != "tcp://from-the-env:2375" {
		t.Errorf("ResolveHost() = %q, want the environment to outrank the context store", got)
	}
}

// TestResolveHostPrefersDockerContextOverTheStore pins the middle rung: an
// explicitly named context beats the one config.json happens to have selected,
// which is what `DOCKER_CONTEXT=x steps ...` has to mean.
func TestResolveHostPrefersDockerContextOverTheStore(t *testing.T) {
	clearDockerEnv(t)
	writeContextStore(t, "selected", map[string]string{
		"selected": "unix:///selected.sock",
		"named":    "unix:///named.sock",
	})
	t.Setenv("DOCKER_CONTEXT", "named")

	got, err := ResolveHost()
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}

	if got != "unix:///named.sock" {
		t.Errorf("ResolveHost() = %q, want the explicitly named context", got)
	}
}

func TestResolveHostReadsTheCurrentContext(t *testing.T) {
	clearDockerEnv(t)
	writeContextStore(t, "colima", map[string]string{"colima": "unix:///home/someone/.colima/docker.sock"})

	got, err := ResolveHost()
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}

	if got != "unix:///home/someone/.colima/docker.sock" {
		t.Errorf("ResolveHost() = %q, want the endpoint of the current context", got)
	}
}

// TestResolveHostDefaultContextIsThePlatformSocket pins the one context name
// with no entry in the store.
//
// `default` is docker's name for "whatever DOCKER_HOST or this platform says",
// and it is written into config.json by `docker context use default` like any
// other. Looking it up would find nothing and report a context that does not
// exist, on a machine that is configured perfectly normally.
func TestResolveHostDefaultContextIsThePlatformSocket(t *testing.T) {
	clearDockerEnv(t)
	writeContextStore(t, "default", nil)

	got, err := ResolveHost()
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}

	if got != DefaultHost {
		t.Errorf("ResolveHost() = %q, want the platform default %q", got, DefaultHost)
	}
}

func TestResolveHostNoConfigIsThePlatformSocket(t *testing.T) {
	clearDockerEnv(t)
	writeContextStore(t, "", nil)

	got, err := ResolveHost()
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}

	if got != DefaultHost {
		t.Errorf("ResolveHost() = %q, want the platform default %q", got, DefaultHost)
	}
}

// TestResolveHostReportsAMissingContext pins that a selected context with no
// entry is an error rather than a silent fall back to the default socket.
//
// The fallback is the dangerous answer: a machine whose context store has been
// moved or half-copied would report "cannot connect to the docker daemon" and
// send the operator to look at a daemon that is running.
func TestResolveHostReportsAMissingContext(t *testing.T) {
	clearDockerEnv(t)
	writeContextStore(t, "vanished", nil)

	_, err := ResolveHost()
	if err == nil {
		t.Fatal("ResolveHost succeeded for a context that is not in the store")
	}

	if !strings.Contains(err.Error(), "vanished") {
		t.Errorf("error = %v, want it to name the context that could not be found", err)
	}
}

// TestResolveHostIgnoresAContextWithoutADockerEndpoint pins a store entry that
// exists but describes something else — a kubernetes-only context is a real
// thing to find in there.
func TestResolveHostIgnoresAContextWithoutADockerEndpoint(t *testing.T) {
	clearDockerEnv(t)

	root := writeContextStore(t, "k8s-only", nil)
	digest := sha256.Sum256([]byte("k8s-only"))
	dir := filepath.Join(root, "contexts", "meta", hex.EncodeToString(digest[:]))

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatalf("making the context directory: %v", err)
	}

	writeJSON(t, filepath.Join(dir, "meta.json"), map[string]any{
		"Name":      "k8s-only",
		"Endpoints": map[string]any{"kubernetes": map[string]any{"Host": "https://cluster"}},
	})

	_, err = ResolveHost()
	if err == nil {
		t.Fatal("ResolveHost succeeded for a context that names no docker endpoint")
	}
}
