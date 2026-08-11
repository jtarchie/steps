package agent

// Tests for running the CLI subprocess inside a container: what command gets
// built, what crosses into it, and what does not. None of these start a
// process or need a docker daemon — the argv and the prepared $HOME are the
// whole contract.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testCLIImage is the image every containerized case runs; its value is
// arbitrary, only its presence matters.
const testCLIImage = "my/claude:1"

// containerPrepared is cliPrepared with an image set, in a real workspace
// directory (ResolveMountPath has to be able to resolve it).
func containerPrepared(t *testing.T) preparedAgentStep {
	t.Helper()

	prepared := cliPrepared(t, []string{"read_file", "run_shell"})
	prepared.ri.Image = testCLIImage
	prepared.conv.env.dir = t.TempDir()

	return prepared
}

// isolateHome points $HOME at a temp dir so a test decides for itself whether
// a credentials file exists, rather than inheriting whatever the machine
// running the suite happens to have logged in.
func isolateHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	return home
}

// writeCredentials creates the file a Linux subscription login leaves behind.
func writeCredentials(t *testing.T, home string) string {
	t.Helper()

	dir := filepath.Join(home, ".claude")
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	path := filepath.Join(dir, ".credentials.json")
	err = os.WriteFile(path, []byte(`{"claudeAiOauth":{}}`), 0o600)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	return path
}

// TestBuildCLICommandHostPathUnchanged: a step with no image: must still
// exec the binary directly, in the workspace, with the allowlisted host env.
func TestBuildCLICommandHostPathUnchanged(t *testing.T) {
	t.Parallel()

	prepared := cliPrepared(t, []string{"read_file"})
	prepared.conv.env.dir = t.TempDir()

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", []string{"--print"}, "")
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	if !strings.HasSuffix(cmd.Path, "claude") {
		t.Errorf("cmd.Path = %q, want the claude binary itself", cmd.Path)
	}

	if cmd.Dir != prepared.conv.env.dir {
		t.Errorf("cmd.Dir = %q, want the workspace %q", cmd.Dir, prepared.conv.env.dir)
	}
}

// TestBuildCLICommandContainerRunsDocker covers the shape of the containerized
// invocation: docker, the image, and the CLI's own argv passed through
// untouched after the separator.
func TestBuildCLICommandContainerRunsDocker(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", []string{"--print", "--model", "sonnet"}, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	if !strings.HasSuffix(cmd.Path, "docker") {
		t.Errorf("cmd.Path = %q, want docker", cmd.Path)
	}

	// cmd.Dir must stay empty: the container's own -w is the working
	// directory, and setting cmd.Dir would only move the docker CLIENT.
	if cmd.Dir != "" {
		t.Errorf("cmd.Dir = %q, want empty for a containerized run", cmd.Dir)
	}

	sep := slices.Index(cmd.Args, "--")
	if sep < 0 {
		t.Fatalf("args = %v, want a -- separator", cmd.Args)
	}

	want := []string{testCLIImage, "claude", "--print", "--model", "sonnet"}
	if got := cmd.Args[sep+1:]; !slices.Equal(got, want) {
		t.Errorf("positionals = %v, want %v", got, want)
	}
}

// TestBuildCLICommandMountsHomeAndSetsIt is the pair that makes the CLI able
// to write its transcript at all: a writable directory, and a HOME pointing
// at it.
func TestBuildCLICommandMountsHomeAndSetsIt(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)
	stepHome := t.TempDir()

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", nil, stepHome)
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	joined := strings.Join(cmd.Args, " ")

	if !strings.Contains(joined, "-v "+stepHome+":"+cliContainerHome+" ") {
		t.Errorf("args = %v, want the step home mounted at %s", cmd.Args, cliContainerHome)
	}

	if !strings.Contains(joined, "-e HOME="+cliContainerHome) {
		t.Errorf("args = %v, want HOME set to %s", cmd.Args, cliContainerHome)
	}
}

// TestBuildCLICommandMountsCredentialsWhenPresent covers the Linux
// subscription-login route, and that it is mounted read-only.
func TestBuildCLICommandMountsCredentialsWhenPresent(t *testing.T) {
	home := isolateHome(t)
	credentials := writeCredentials(t, home)

	prepared := containerPrepared(t)

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	want := "-v " + credentials + ":" + cliContainerHome + "/.claude/.credentials.json:ro"
	if !strings.Contains(strings.Join(cmd.Args, " "), want) {
		t.Errorf("args = %v, want to contain %q", cmd.Args, want)
	}
}

// TestBuildCLICommandOmitsCredentialsWhenAbsent is the macOS case: nothing to
// mount, and the run must still be well-formed rather than mounting a path
// that does not exist (docker would create a DIRECTORY there, and the CLI
// would then find an unreadable credentials "file").
func TestBuildCLICommandOmitsCredentialsWhenAbsent(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	if strings.Contains(strings.Join(cmd.Args, " "), ".credentials.json") {
		t.Errorf("args = %v, want no credentials mount when the file does not exist", cmd.Args)
	}
}

// TestBuildCLICommandDoesNotMountTheWholeClaudeDir is the privacy property:
// only the one credentials file crosses, never the directory holding the
// operator's history, transcripts and settings.
func TestBuildCLICommandDoesNotMountTheWholeClaudeDir(t *testing.T) {
	home := isolateHome(t)
	writeCredentials(t, home)

	prepared := containerPrepared(t)

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	for _, unwanted := range []string{
		"-v " + filepath.Join(home, ".claude") + ":",
		"-v " + home + ":",
	} {
		if strings.Contains(strings.Join(cmd.Args, " "), unwanted) {
			t.Errorf("args = %v, must not mount %q", cmd.Args, unwanted)
		}
	}
}

// TestBuildCLICommandForwardsAPIKeyByNameOnly: the key reaches the container,
// but its VALUE must never appear in the docker client's argv, where the
// host's process list would expose it.
func TestBuildCLICommandForwardsAPIKeyByNameOnly(t *testing.T) {
	isolateHome(t)
	t.Setenv("MY_KEY", "sk-secret-value")

	prepared := containerPrepared(t)
	prepared.ri.APIKeyEnv = "MY_KEY"

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	joined := strings.Join(cmd.Args, " ")

	if strings.Contains(joined, "sk-secret-value") {
		t.Fatalf("args = %v, leaked the key value into argv", cmd.Args)
	}

	if !strings.Contains(joined, "-e ANTHROPIC_API_KEY") {
		t.Errorf("args = %v, want ANTHROPIC_API_KEY forwarded by name", cmd.Args)
	}

	// The docker client's own environment is what supplies the value.
	if !slices.Contains(cmd.Env, "ANTHROPIC_API_KEY=sk-secret-value") {
		t.Errorf("cmd.Env does not carry the key for the docker client to forward")
	}
}

// TestNewCLIStepHomePreCreatesClaudeDir pins the reason the directory is made
// host-side: docker would otherwise create the bind-mount parent as root, and
// on Linux the container runs as the host uid, which could then not write its
// own transcript.
func TestNewCLIStepHomePreCreatesClaudeDir(t *testing.T) {
	t.Parallel()

	home, err := newCLIStepHome()
	if err != nil {
		t.Fatalf("newCLIStepHome: %v", err)
	}

	defer func() { _ = os.RemoveAll(home) }()

	info, err := os.Stat(filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatalf("stat .claude: %v", err)
	}

	if !info.IsDir() {
		t.Fatalf(".claude is not a directory")
	}

	if perm := info.Mode().Perm(); perm != cliStepHomeMode {
		t.Errorf(".claude mode = %o, want %o (the container uid need not be ours)", perm, cliStepHomeMode)
	}
}

// TestProbeCLICredentialsRoutes covers preflight's machine-dependent check:
// either route satisfies it, neither is a problem that names both.
func TestProbeCLICredentialsRoutes(t *testing.T) {
	t.Run("credentials file", func(t *testing.T) {
		home := isolateHome(t)
		writeCredentials(t, home)

		ri := containerPrepared(t).ri
		err := probeCLICredentials(ri)
		if err != nil {
			t.Errorf("probeCLICredentials: %v, want nil when the credentials file exists", err)
		}
	})

	t.Run("api key env", func(t *testing.T) {
		isolateHome(t)
		t.Setenv("MY_KEY", "sk-value")

		ri := containerPrepared(t).ri
		ri.APIKeyEnv = "MY_KEY"

		err := probeCLICredentials(ri)
		if err != nil {
			t.Errorf("probeCLICredentials: %v, want nil when api_key_env is set", err)
		}
	})

	t.Run("neither", func(t *testing.T) {
		isolateHome(t)

		ri := containerPrepared(t).ri

		err := probeCLICredentials(ri)
		if err == nil {
			t.Fatal("probeCLICredentials: nil, want a problem when neither route exists")
		}

		// The message has to name both routes, since which one applies
		// depends on the operating system.
		for _, want := range []string{"credentials.json", "Keychain", "api_key_env"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("named but unexported key", func(t *testing.T) {
		isolateHome(t)

		ri := containerPrepared(t).ri
		ri.APIKeyEnv = "NOT_EXPORTED"

		err := probeCLICredentials(ri)
		if err == nil {
			t.Fatal("probeCLICredentials: nil, want a problem")
		}

		// Naming a variable nobody exported is a different mistake from
		// naming none, and the message should say which happened.
		if !strings.Contains(err.Error(), "NOT_EXPORTED") {
			t.Errorf("error %q does not name the variable that is missing", err)
		}
	})
}

// TestBuildCLICommandNamesTheContainer covers the property that makes a
// timed-out step recoverable. Killing the docker client does not stop the
// container it started, so without a name of our own there is nothing to
// `docker rm -f` — the CLI would keep running, keep spending, and keep
// writing into the workspace the next step is about to read.
func TestBuildCLICommandNamesTheContainer(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)

	cmd, container, err := buildCLICommand(t.Context(), prepared, "claude", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	if container == "" {
		t.Fatal("no container name returned; a canceled step could not reclaim it")
	}

	if !strings.Contains(strings.Join(cmd.Args, " "), "--name "+container) {
		t.Errorf("args = %v, want --name %s", cmd.Args, container)
	}

	// Cancellation must reach the client as SIGTERM rather than an immediate
	// kill, matching shell.dockerCommand, so it can detach cleanly.
	if cmd.Cancel == nil {
		t.Error("cmd.Cancel is nil; a canceled context would SIGKILL the docker client outright")
	}

	// The host path has no container, so nothing to name or reclaim.
	hostPrepared := cliPrepared(t, []string{"read_file"})
	hostPrepared.conv.env.dir = t.TempDir()

	_, hostContainer, err := buildCLICommand(t.Context(), hostPrepared, "claude", nil, "")
	if err != nil {
		t.Fatalf("buildCLICommand (host): %v", err)
	}

	if hostContainer != "" {
		t.Errorf("host path returned container %q, want none", hostContainer)
	}
}

// TestBuildCLICommandDoesNotForwardAmbientKey: naming an api_key_env: that
// nobody exported must not silently fall through to whatever key this
// process happens to carry. Doing so would authenticate the run against the
// operator's personal account while the pipeline says otherwise, and the
// host path cannot do it (shell.HostEnv's allowlist excludes the variable),
// so the two paths would disagree.
func TestBuildCLICommandDoesNotForwardAmbientKey(t *testing.T) {
	isolateHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-personal-key")

	prepared := containerPrepared(t)
	prepared.ri.APIKeyEnv = "NOT_EXPORTED"

	cmd, _, err := buildCLICommand(t.Context(), prepared, "claude", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildCLICommand: %v", err)
	}

	if strings.Contains(strings.Join(cmd.Args, " "), "-e ANTHROPIC_API_KEY") {
		t.Errorf("args = %v, forwarded the ambient key for an unexported api_key_env", cmd.Args)
	}
}
