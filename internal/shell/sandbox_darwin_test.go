package shell

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fencedRunner(t *testing.T, cwd, network string) Runner {
	t.Helper()

	runner, err := NewRunner(RunnerSpec{Cwd: cwd, Sandbox: true, Network: network})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	return runner
}

// Reads stay open (the control) except the credential paths, and a symlinked credential dir is denied by the path the kernel actually opens.
func TestSandboxDeniesCredentialReadsOnly(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()

	for path, body := range map[string]string{
		home + "/.ssh/id_ed25519":         "key",
		elsewhere + "/aws/credentials":    "secret",
		home + "/notes.txt":               "fine",
		home + "/.docker/contexts/marker": "fine",
	} {
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path, []byte(body), 0o600)
		}

		if err != nil {
			t.Fatal(err)
		}
	}

	err := os.Symlink(elsewhere+"/aws", home+"/.aws")
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)

	runner := fencedRunner(t, t.TempDir(), "")

	for path, allowed := range map[string]bool{
		"$HOME/.ssh/id_ed25519":         false,
		"$HOME/.aws/credentials":        false,
		"$HOME/notes.txt":               true,
		"$HOME/.docker/contexts/marker": true,
	} {
		checkRead(t, runner, path, allowed)
	}
}

func checkRead(t *testing.T, runner Runner, path string, allowed bool) {
	t.Helper()

	stdout, stderr, code, err := RunCaptureFull(context.Background(), runner, "cat "+path)
	if err != nil {
		t.Fatalf("cat %s: %v", path, err)
	}

	switch {
	case allowed && code != 0:
		t.Errorf("cat %s exited %d (%s): reads outside the credential list must stay open", path, code, stderr)
	case !allowed && (code == 0 || !strings.Contains(stderr, "Operation not permitted")):
		t.Errorf("cat %s = %q, exit %d, stderr %q: want the sandbox to deny it", path, stdout, code, stderr)
	}
}

func TestSandboxNetworkIsCutOnlyByNetworkNone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("reached")) }))
	defer server.Close()

	for _, tc := range []struct {
		network string
		reach   bool
	}{{"", true}, {"none", false}} {
		stdout, _, _, err := RunCaptureFull(context.Background(), fencedRunner(t, t.TempDir(), tc.network), "curl -s -m 3 "+server.URL)
		if err != nil {
			t.Fatal(err)
		}

		if got := stdout == "reached"; got != tc.reach {
			t.Errorf("network %q: reached = %v, want %v", tc.network, got, tc.reach)
		}
	}
}

func TestSandboxTMPDIRIsPrivatePerRunnerAndGoneAfterClose(t *testing.T) {
	tmpdirOf := func(runner Runner) string {
		out, err := RunCapture(context.Background(), runner, `printf %s "$TMPDIR"`)
		if err != nil {
			t.Fatal(err)
		}

		return string(out)
	}

	first, err := NewRunner(RunnerSpec{Cwd: t.TempDir(), Sandbox: true})
	if err != nil {
		t.Fatal(err)
	}

	second := fencedRunner(t, t.TempDir(), "")

	dir := tmpdirOf(first.WithLabel("labelled copy"))
	if dir == tmpdirOf(second) {
		t.Errorf("two fenced runners share TMPDIR %s: one step could write into the other's", dir)
	}

	err = first.Close()
	if err != nil {
		t.Fatal(err)
	}

	_, err = os.Stat(dir)
	if !os.IsNotExist(err) {
		t.Errorf("TMPDIR %s survived Close: %v", dir, err)
	}
}
