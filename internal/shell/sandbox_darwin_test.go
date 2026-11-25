package shell

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
		home + "/.ssh/id_ed25519":                    "key",
		elsewhere + "/aws/credentials":               "secret",
		home + "/notes.txt":                          "fine",
		home + "/.config/git/config":                 "fine",
		home + "/.docker/contexts/meta/x/meta.json":  "endpoint",
		home + "/.git-credentials":                   "secret",
		home + "/.config/git/credentials":            "secret",
		home + "/.npmrc":                             "secret",
		home + "/.pypirc":                            "secret",
		home + "/.cargo/credentials":                 "secret",
		home + "/.cargo/credentials.toml":            "secret",
		home + "/.azure/msal_token_cache.json":       "secret",
		home + "/.terraform.d/credentials.tfrc.json": "secret",
		home + "/.config/op/config":                  "secret",
		home + "/.pgpass":                            "secret",
		home + "/.config/steps/private/mcp.json":     "secret",
		home + "/.config/steps/notes.txt":            "fine",
		home + "/.claude/.credentials.json":          "secret",
		home + "/.claude/settings.json":              "fine",
		home + "/.claude.json":                       "secret",
		home + "/.codex/auth.json":                   "secret",
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
		"$HOME/.ssh/id_ed25519":                    false,
		"$HOME/.aws/credentials":                   false,
		"$HOME/notes.txt":                          true,
		"$HOME/.config/git/config":                 true,
		"$HOME/.docker/contexts/meta/x/meta.json":  false,
		"$HOME/.git-credentials":                   false,
		"$HOME/.config/git/credentials":            false,
		"$HOME/.npmrc":                             false,
		"$HOME/.pypirc":                            false,
		"$HOME/.cargo/credentials":                 false,
		"$HOME/.cargo/credentials.toml":            false,
		"$HOME/.azure/msal_token_cache.json":       false,
		"$HOME/.terraform.d/credentials.tfrc.json": false,
		"$HOME/.config/op/config":                  false,
		"$HOME/.pgpass":                            false,
		"$HOME/.config/steps/private/mcp.json":     false,
		"$HOME/.config/steps/notes.txt":            true,
		"$HOME/.claude/.credentials.json":          false,
		"$HOME/.claude/settings.json":              true,
		"$HOME/.claude.json":                       false,
		"$HOME/.codex/auth.json":                   false,
	} {
		checkRead(t, runner, path, allowed)
	}
}

// The deny rules are path strings, so what holds them is the kernel matching the file's real path: another spelling of it on a case-insensitive volume, the firmlink above it, or a hard link the command makes in its own workspace must not be a second name that reads (measured on macOS 27.0.1; link(2) itself is what is refused).
func TestSandboxDeniesACredentialUnderAnotherNameForIt(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	err := os.MkdirAll(home+"/.ssh", 0o700)
	if err == nil {
		err = os.WriteFile(home+"/.ssh/id_ed25519", []byte("key"), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)

	runner := fencedRunner(t, cwd, "")

	scripts := []string{
		"cat /System/Volumes/Data$HOME/.ssh/id_ed25519",
		"ln $HOME/.ssh/id_ed25519 ./k; cat ./k",
		"ln -s $HOME/.ssh/id_ed25519 ./s; cat ./s",
	}

	_, err = os.Stat(home + "/.SSH/ID_ED25519")
	if err == nil {
		scripts = append(scripts, "cat $HOME/.SSH/id_ed25519", "cat $HOME/.Ssh/ID_ED25519")
	}

	for _, script := range scripts {
		stdout, stderr, code, err := RunCaptureFull(context.Background(), runner, script)
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}

		if code == 0 || strings.Contains(stdout, "key") {
			t.Errorf("%s = %q, exit %d, stderr %q: want the sandbox to deny it", script, stdout, code, stderr)
		}
	}

	_, err = os.Lstat(cwd + "/k")
	if !os.IsNotExist(err) {
		t.Errorf("the fenced command hard-linked a credential into its workspace: %v", err)
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

func TestSeatbeltProfileDeniesLaunchServices(t *testing.T) {
	t.Parallel()

	argv, err := seatbelt{}.command(sandboxPolicy{network: true}, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}

	if profile := argv[slices.Index(argv, "-p")+1]; !strings.Contains(profile, "(deny lsopen)") {
		t.Errorf("profile %s does not deny lsopen: `open` would start an app outside the fence", profile)
	}
}

// LaunchServices starts an app outside the sandbox that asked, so an app a fenced command builds in its own workspace is its way out. The app here only touches a marker, and is never started while the rule holds.
func TestSandboxCannotStartAnAppThroughLaunchServices(t *testing.T) {
	cwd := mustResolve(t, t.TempDir())
	marker := filepath.Join(cwd, "launched")
	macos := filepath.Join(cwd, "Probe.app", "Contents", "MacOS")

	err := os.MkdirAll(macos, 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(macos, "probe"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700) //nolint:gosec // the bundle's executable
	}

	if err == nil {
		err = os.WriteFile(filepath.Join(cwd, "Probe.app", "Contents", "Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>probe</string>
<key>CFBundleIdentifier</key><string>dev.stepsci.test.fence-probe</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSBackgroundOnly</key><true/>
</dict></plist>`), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	_, stderr, code, err := RunCaptureFull(context.Background(), fencedRunner(t, cwd, ""), "open -g -W Probe.app")
	if err != nil {
		t.Fatal(err)
	}

	if code == 0 {
		t.Errorf("open exited 0 (%s): LaunchServices took the request from inside the fence", stderr)
	}

	_, err = os.Stat(marker)
	if !os.IsNotExist(err) {
		t.Errorf("the app ran outside the fence and wrote %s: %v", marker, err)
	}
}

// A fenced command can read this process's environment on macOS, the key that unseals the database's logins included, so the database is what the fence keeps from it: the file and sqlite's two sidecars, and nothing else beside them. The layout is a daemon's default, with the step's tree under workspace/ beside the database.
func TestSandboxDeniesReadingTheStateDatabase(t *testing.T) {
	root := mustResolve(t, t.TempDir())
	build := filepath.Join(root, ".steps", "workspace", "build")

	err := os.MkdirAll(build, 0o700)
	for _, file := range []string{".steps/steps.db", ".steps/steps.db-wal", ".steps/steps.db-shm", ".steps/steps.db.bak", ".steps/workspace/cached", "pipeline.yml"} {
		if err == nil {
			err = os.WriteFile(filepath.Join(root, file), []byte("row"), 0o600)
		}
	}

	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	hideState(t, ".steps/steps.db")

	runner := fencedRunner(t, build, "")

	for path, allowed := range map[string]bool{
		root + "/.steps/steps.db":         false,
		root + "/.steps/steps.db-wal":     false,
		root + "/.steps/steps.db-shm":     false,
		"../../steps.db":                  false,
		root + "/.steps/steps.db.bak":     true,
		root + "/.steps/workspace/cached": true,
		root + "/pipeline.yml":            true,
	} {
		checkRead(t, runner, path, allowed)
	}

	_, _, code, err := RunCaptureFull(context.Background(), runner, "echo row > kept && cat kept")
	if err != nil || code != 0 {
		t.Errorf("the step's own tree beside the database is not writable: exit %d, %v", code, err)
	}
}

// A database not made yet, as on a first run, must not keep a fenced command from starting.
func TestSandboxRunsWhenTheStateDatabaseIsMissing(t *testing.T) {
	root := mustResolve(t, t.TempDir())
	hideState(t, filepath.Join(root, "nowhere", "steps.db"))

	err := os.WriteFile(filepath.Join(root, "notes"), []byte("fine"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	checkRead(t, fencedRunner(t, t.TempDir(), ""), root+"/notes", true)
}
