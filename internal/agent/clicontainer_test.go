package agent

// Tests for running the CLI inside a container: what container gets built,
// what crosses into it, and what does not. None of these start anything or
// need a docker daemon.
//
// They used to read an argument vector, because the run was a `docker run`
// subprocess and its argv was the only artefact. It is a container created
// through the engine API now, so they read the SPEC — which is the same
// contract one indirection earlier, and a better oracle: a mount is a mount
// rather than a substring, and a value that is present is present rather than
// absent-from-a-joined-string.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// containerSpecFor builds the containerized run and returns the spec it would
// create, failing the test if the host path was taken instead.
func containerSpecFor(t *testing.T, prepared preparedAgentStep, args []string, stepHome string) (shell.DockerRunSpec, string) {
	t.Helper()

	process, container, err := buildCLIProcess(t.Context(), prepared, "claude", args, stepHome)
	if err != nil {
		t.Fatalf("buildCLIProcess: %v", err)
	}

	containerized, ok := process.(*containerCLIProcess)
	if !ok {
		t.Fatalf("process is %T, want a containerized one", process)
	}

	return containerized.spec, container
}

// mountedAt returns the container path spec binds hostPath at, and whether it
// is read-only. Reported rather than matched, so a failure says what the
// mount actually was.
func mountedAt(spec shell.DockerRunSpec, hostPath string) (containerPath string, readOnly, found bool) {
	for _, mount := range spec.ExtraMounts {
		if mount.HostPath == hostPath {
			return mount.ContainerPath, mount.ReadOnly, true
		}
	}

	return "", false, false
}

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

	process, _, err := buildCLIProcess(t.Context(), prepared, "claude", []string{"--print"}, "")
	if err != nil {
		t.Fatalf("buildCLIProcess: %v", err)
	}

	host, ok := process.(*hostCLIProcess)
	if !ok {
		t.Fatalf("process is %T, want a host one", process)
	}

	if !strings.HasSuffix(host.cmd.Path, "claude") {
		t.Errorf("cmd.Path = %q, want the claude binary itself", host.cmd.Path)
	}

	if host.cmd.Dir != prepared.conv.env.dir {
		t.Errorf("cmd.Dir = %q, want the workspace %q", host.cmd.Dir, prepared.conv.env.dir)
	}
}

// TestBuildCLICommandContainerRunsTheImage covers the shape of the
// containerized run: the image, and the CLI's own argv reaching it untouched.
//
// Untouched is the whole point and the reason there is no `sh -c` anywhere
// here: an argument containing spaces, quotes or JSON braces —
// --append-system-prompt is exactly that — is one argument, and a shell in
// between is where it would come apart.
func TestBuildCLICommandContainerRunsTheImage(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)

	spec, _ := containerSpecFor(t, prepared, []string{"--print", "--model", "sonnet"}, t.TempDir())

	if spec.Image != testCLIImage {
		t.Errorf("image = %q, want %q", spec.Image, testCLIImage)
	}

	want := []string{"claude", "--print", "--model", "sonnet"}
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("argv = %v, want %v", spec.Argv, want)
	}

	if spec.ResolvedCwd != resolved(t, prepared.conv.env.dir) {
		t.Errorf("cwd = %q, want the workspace", spec.ResolvedCwd)
	}
}

// resolved is the workspace path as the daemon will be told it.
func resolved(t *testing.T, dir string) string {
	t.Helper()

	path, err := shell.ResolveMountPath(dir)
	if err != nil {
		t.Fatalf("ResolveMountPath: %v", err)
	}

	return path
}

// TestBuildCLICommandMountsHomeAndSetsIt is the pair that makes the CLI able
// to write its transcript at all: a writable directory, and a HOME pointing
// at it.
func TestBuildCLICommandMountsHomeAndSetsIt(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)
	stepHome := t.TempDir()

	spec, _ := containerSpecFor(t, prepared, nil, stepHome)

	at, readOnly, found := mountedAt(spec, stepHome)
	if !found {
		t.Fatalf("mounts = %v, want the step home mounted", spec.ExtraMounts)
	}

	if at != cliContainerHome {
		t.Errorf("step home mounted at %q, want %q", at, cliContainerHome)
	}

	if readOnly {
		t.Error("the step home is mounted read-only; the CLI could not write its transcript")
	}

	if spec.ExtraEnv["HOME"] != cliContainerHome {
		t.Errorf("HOME = %q, want %q", spec.ExtraEnv["HOME"], cliContainerHome)
	}
}

// TestBuildCLICommandMountsCredentialsWhenPresent covers the Linux
// subscription-login route, and that it is mounted read-only.
func TestBuildCLICommandMountsCredentialsWhenPresent(t *testing.T) {
	home := isolateHome(t)
	credentials := writeCredentials(t, home)

	prepared := containerPrepared(t)

	spec, _ := containerSpecFor(t, prepared, nil, t.TempDir())

	at, readOnly, found := mountedAt(spec, credentials)
	if !found {
		t.Fatalf("mounts = %v, want the credentials file mounted", spec.ExtraMounts)
	}

	if at != cliContainerHome+"/.claude/.credentials.json" {
		t.Errorf("credentials mounted at %q, want the CLI's own path", at)
	}

	// Read-only bounds a hostile image to READING the one token it was
	// deliberately given.
	if !readOnly {
		t.Error("the credentials file is mounted writable, want read-only")
	}
}

// TestBuildCLICommandOmitsCredentialsWhenAbsent is the macOS case: nothing to
// mount, and the run must still be well-formed rather than mounting a path
// that does not exist (docker would create a DIRECTORY there, and the CLI
// would then find an unreadable credentials "file").
func TestBuildCLICommandOmitsCredentialsWhenAbsent(t *testing.T) {
	isolateHome(t)

	prepared := containerPrepared(t)

	spec, _ := containerSpecFor(t, prepared, nil, t.TempDir())

	for _, mount := range spec.ExtraMounts {
		if strings.Contains(mount.HostPath, ".credentials.json") {
			t.Errorf("mounts = %v, want no credentials mount when the file does not exist", spec.ExtraMounts)
		}
	}
}

// TestBuildCLICommandDoesNotMountTheWholeClaudeDir is the privacy property:
// only the one credentials file crosses, never the directory holding the
// operator's history, transcripts and settings.
func TestBuildCLICommandDoesNotMountTheWholeClaudeDir(t *testing.T) {
	home := isolateHome(t)
	writeCredentials(t, home)

	prepared := containerPrepared(t)

	spec, _ := containerSpecFor(t, prepared, nil, t.TempDir())

	for _, unwanted := range []string{filepath.Join(home, ".claude"), home} {
		if _, _, found := mountedAt(spec, unwanted); found {
			t.Errorf("mounts = %v, must not mount %q", spec.ExtraMounts, unwanted)
		}
	}
}

// TestBuildCLICommandCarriesTheAPIKeyUnderTheCLIsName: whatever the pipeline
// called its api_key_env:, the value reaches the container as the variable the
// CLI actually reads.
//
// This used to also assert the value stayed OUT of an argv, because the run
// was a docker command line the host's process list could read. There is no
// command line now — the value travels in a request body — so what is left to
// pin is that it arrives, and arrives renamed.
func TestBuildCLICommandCarriesTheAPIKeyUnderTheCLIsName(t *testing.T) {
	isolateHome(t)
	t.Setenv("MY_KEY", "sk-secret-value")

	prepared := containerPrepared(t)
	prepared.ri.APIKeyEnv = "MY_KEY"

	spec, _ := containerSpecFor(t, prepared, nil, t.TempDir())

	if spec.ExtraEnv[cliAPIKeyEnv] != "sk-secret-value" {
		t.Errorf("%s = %q, want the value of the pipeline's api_key_env", cliAPIKeyEnv, spec.ExtraEnv[cliAPIKeyEnv])
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

	spec, container := containerSpecFor(t, prepared, nil, t.TempDir())

	if container == "" {
		t.Fatal("no container name returned; a canceled step could not reclaim it")
	}

	// The name the caller was given has to be the name the container gets, or
	// the reclaim removes nothing and reports success.
	if spec.Name != container {
		t.Errorf("spec names %q but the caller was told %q; the reclaim would miss it", spec.Name, container)
	}

	// The host path has no container, so nothing to name or reclaim.
	hostPrepared := cliPrepared(t, []string{"read_file"})
	hostPrepared.conv.env.dir = t.TempDir()

	_, hostContainer, err := buildCLIProcess(t.Context(), hostPrepared, "claude", nil, "")
	if err != nil {
		t.Fatalf("buildCLIProcess (host): %v", err)
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

	spec, _ := containerSpecFor(t, prepared, nil, t.TempDir())

	if _, carried := spec.ExtraEnv[cliAPIKeyEnv]; carried {
		t.Errorf("%s = %q, carried the ambient key for an unexported api_key_env",
			cliAPIKeyEnv, spec.ExtraEnv[cliAPIKeyEnv])
	}

	// And nothing else smuggled it in either: the container's environment must
	// not contain that value at all.
	for name, value := range spec.ExtraEnv {
		if value == "sk-ambient-personal-key" {
			t.Errorf("%s carries the operator's ambient key", name)
		}
	}
}
