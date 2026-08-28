package agent

// Docker-backed tests for the containerized CLI run. These answer the
// questions the argv tests structurally cannot: whether the command we build
// actually starts, whether the container can write where we told it its HOME
// is, whether it can reach the bridge this process hosts, and whether a
// canceled run leaves anything behind.
//
// Opt-in and non-hermetic, following internal/shell's docker_integration_test
// precedent — a plain `go test ./...` skips every one of them.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// requireDockerAgent skips the calling test unless a usable daemon with the
// test image is reachable. Not opt-in: a test guarding a shipped feature does
// not get to be optional.
func requireDockerAgent(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = exec.CommandContext(ctx, "docker", "info").Run()
	if err != nil {
		t.Skip("docker daemon not reachable (`docker info` failed)")
	}

	err = exec.CommandContext(ctx, "docker", "image", "inspect", cliTestImage).Run()
	if err != nil {
		t.Skipf("test image %s not present locally (docker pull %s)", cliTestImage, cliTestImage)
	}
}

// cliTestImage stands in for an image with the CLI installed. Its `sh` and
// `wget` are all these tests need, and the run path never assumes more of a
// real CLI image than that it can exec the binary it was given.
const cliTestImage = "alpine:3"

// dockerEndpoint returns the active context's docker host, or "" if the CLI
// will not say.
//
// It must be read BEFORE any test moves HOME: the docker CLI finds its active
// context under $HOME/.docker, so a blank HOME silently drops a non-default
// endpoint (colima, Docker Desktop, a remote host) and falls back to
// /var/run/docker.sock — which on a Mac does not exist. Production never hits
// this, because the docker client there inherits the real environment; it is
// purely a consequence of these tests isolating HOME to control whether a
// credentials file exists.
func dockerEndpoint(t *testing.T) string {
	t.Helper()

	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}

	out, err := exec.CommandContext(t.Context(), "docker", "context", "inspect",
		"--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

// useSharedTempRoot points TMPDIR at a directory the docker daemon can
// actually see, and returns it.
//
// This is not test scaffolding around a test-only problem. When the daemon
// runs in a VM (Docker Desktop, colima) only certain host paths are shared
// into it — the user's home is, and macOS's own $TMPDIR (/var/folders/...)
// is not. A bind mount of an unshared path does not fail: docker creates an
// empty directory at the target instead, so the container writes into a
// phantom that no one on the host ever reads, and a mounted FILE arrives as
// a DIRECTORY. Every temp directory these tests hand to docker therefore has
// to live somewhere shared, which is exactly the constraint a real run is
// under (see docs/infra.md on TMPDIR).
func useSharedTempRoot(t *testing.T) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to root a daemon-visible temp dir in")
	}

	root, err := os.MkdirTemp(home, ".steps-test-*")
	if err != nil {
		t.Skipf("cannot create a daemon-visible temp dir under %s: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMPDIR", root)

	return root
}

// containerized builds a prepared step pointed at the test image, with a
// workspace and an isolated HOME, both in daemon-visible storage.
func containerized(t *testing.T) (preparedAgentStep, string) {
	t.Helper()

	// Both of these read the REAL environment, so they must run before
	// anything moves HOME or TMPDIR.
	endpoint := dockerEndpoint(t)
	useSharedTempRoot(t)

	// Now that TMPDIR is daemon-visible, the temp HOME this creates is too —
	// which is what lets the credentials mount be a real file.
	isolateHome(t)

	// DOCKER_HOST takes precedence over the context file the isolated HOME
	// can no longer reach.
	if endpoint != "" {
		t.Setenv("DOCKER_HOST", endpoint)
	}

	prepared := cliPrepared(t, []string{"read_file"})
	prepared.ri.Image = cliTestImage
	prepared.conv.env.dir = t.TempDir()

	home, err := newCLIStepHome()
	if err != nil {
		t.Fatalf("newCLIStepHome: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(home) })

	return prepared, home
}

// runContainer builds and runs the command, returning its combined output.
func runContainer(t *testing.T, prepared preparedAgentStep, home, binary string, args []string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	process, container, err := buildCLIProcess(ctx, prepared, binary, args, home)
	if err != nil {
		t.Fatalf("buildCLIProcess: %v", err)
	}

	t.Cleanup(func() { shell.RemoveContainer(context.Background(), container) })

	var stderr strings.Builder

	// An empty stdin rather than none, matching production: the CLI is always
	// fed a prompt, and a container attached without one would leave anything
	// that reads stdin waiting instead of seeing an end of file.
	stdout, err := process.Start(ctx, strings.NewReader(""), &stderr)
	if err != nil {
		t.Fatalf("starting the container: %v", err)
	}

	out, readErr := io.ReadAll(stdout)
	if readErr != nil {
		t.Fatalf("reading the container's output: %v", readErr)
	}

	runErr := process.Wait(ctx)
	if runErr != nil {
		runErr = fmt.Errorf("%w", runErr)
	}

	// Combined, because these tests assert on what the container SAID and
	// several of the commands they run report through stderr.
	return string(out) + stderr.String(), runErr
}

// TestCLIContainerIntegrationRunsArgvIntact is the payoff of not going
// through `sh -c`: an argument containing spaces, quotes and JSON braces
// reaches the binary as ONE argument, byte for byte. Under a shell wrapper
// this is where --append-system-prompt would come apart.
func TestCLIContainerIntegrationRunsArgvIntact(t *testing.T) {
	requireDockerAgent(t)

	prepared, home := containerized(t)

	awkward := `{"role": "system", "text": "a b  c 'quoted' $HOME"}`

	out, err := runContainer(t, prepared, home, "echo", []string{awkward})
	if err != nil {
		t.Fatalf("running the container: %v (output %q)", err, out)
	}

	if strings.TrimSpace(out) != awkward {
		t.Errorf("container printed %q, want %q byte for byte", strings.TrimSpace(out), awkward)
	}
}

// TestCLIContainerIntegrationHomeIsWritable is the assumption the whole
// $HOME design rests on and that no argv test can reach: the container can
// create the transcript directory under the HOME we mounted, whatever uid it
// runs as. If docker created that mount parent root-owned, or the mode were
// too narrow, this is where it shows up.
func TestCLIContainerIntegrationHomeIsWritable(t *testing.T) {
	requireDockerAgent(t)

	prepared, home := containerized(t)

	out, err := runContainer(t, prepared, home, "sh",
		[]string{"-c", `mkdir -p "$HOME/.claude/projects" && echo written > "$HOME/.claude/projects/session.jsonl"`})
	if err != nil {
		t.Fatalf("container could not write under its HOME: %v (output %q)", err, out)
	}

	// Host-side: the transcript is inside the per-step directory, which is
	// what makes removing that directory the whole of cleanup.
	//nolint:gosec // a path this test just created under its own temp dir
	written, err := os.ReadFile(filepath.Join(home, ".claude", "projects", "session.jsonl"))
	if err != nil {
		t.Fatalf("reading what the container wrote: %v", err)
	}

	if strings.TrimSpace(string(written)) != "written" {
		t.Errorf("transcript = %q, want %q", written, "written")
	}
}

// TestCLIContainerIntegrationHomeIsSet checks the CLI would actually look in
// the directory we mounted: HOME has to be the container path, not whatever
// the image's user record says.
func TestCLIContainerIntegrationHomeIsSet(t *testing.T) {
	requireDockerAgent(t)

	prepared, home := containerized(t)

	out, err := runContainer(t, prepared, home, "sh", []string{"-c", `printf %s "$HOME"`})
	if err != nil {
		t.Fatalf("running the container: %v (output %q)", err, out)
	}

	if strings.TrimSpace(out) != cliContainerHome {
		t.Errorf("HOME in container = %q, want %q", strings.TrimSpace(out), cliContainerHome)
	}
}

// TestCLIContainerIntegrationCredentialsAreReadableAndReadOnly covers both
// halves of the credentials mount: the CLI can read the token, and a hostile
// image cannot rewrite the operator's file through it.
func TestCLIContainerIntegrationCredentialsAreReadableAndReadOnly(t *testing.T) {
	requireDockerAgent(t)

	prepared, home := containerized(t)

	// isolateHome ran inside containerized, so this writes into a temp HOME.
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	writeCredentials(t, hostHome)

	out, runErr := runContainer(t, prepared, home, "sh",
		[]string{"-c", `cat "$HOME/.claude/.credentials.json"; echo tampered >> "$HOME/.claude/.credentials.json" 2>/dev/null && echo WRITABLE || echo READONLY`})
	if runErr != nil {
		t.Fatalf("running the container: %v (output %q)", runErr, out)
	}

	if !strings.Contains(out, "claudeAiOauth") {
		t.Errorf("container could not read the credentials it was given: %q", out)
	}

	if !strings.Contains(out, "READONLY") {
		t.Errorf("credentials mount is writable from inside the container: %q", out)
	}

	// And the host file is untouched.
	//nolint:gosec // a path this test just created under its own temp HOME
	after, err := os.ReadFile(filepath.Join(hostHome, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatalf("reading the host credentials back: %v", err)
	}

	if strings.Contains(string(after), "tampered") {
		t.Error("the container modified the host credentials file")
	}
}

// TestCLIContainerIntegrationBridgeIsReachable is the load-bearing
// networking assumption, and the one that cannot be checked without a
// daemon: a container started this way can reach the MCP bridge this process
// is hosting. If it cannot, every bridged tool call fails — including the
// verdict, which is how the step routes at all.
func TestCLIContainerIntegrationBridgeIsReachable(t *testing.T) {
	requireDockerAgent(t)

	prepared, home := containerized(t)

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, nil, nil), nil, cliBridgeReach(prepared.ri))
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	// Unauthenticated on purpose: a 401 proves the request reached this
	// process's HTTP server, which is the whole question. Anything that
	// cannot route would fail to connect instead.
	out, runErr := runContainer(t, prepared, home, "sh",
		[]string{"-c", `wget -S -T 10 -O /dev/null "` + bridge.url + `" 2>&1 | head -20; echo "exit=$?"`})
	if runErr != nil {
		t.Fatalf("running the container: %v (output %q)", runErr, out)
	}

	if !strings.Contains(out, "401") {
		t.Errorf("container did not reach the bridge at %s\noutput: %s", bridge.url, out)
	}
}

// TestCLIContainerIntegrationRemoveContainerStopsIt is the regression test
// for a step that outlives its timeout.
//
// It used to be phrased about the docker CLIENT: killing it did not stop the
// container it started. There is no client process any more, and the property
// survived the change intact — abandoning THIS end, by cancelling the context
// the run was started with, stops nothing on the daemon. The container keeps
// running, keeps spending, and keeps writing into the bind-mounted workspace
// the next step is about to read. Only the name reclaims it.
func TestCLIContainerIntegrationRemoveContainerStopsIt(t *testing.T) {
	requireDockerAgent(t)

	prepared, home := containerized(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	process, container, err := buildCLIProcess(ctx, prepared, "sleep", []string{"300"}, home)
	if err != nil {
		t.Fatalf("buildCLIProcess: %v", err)
	}

	t.Cleanup(func() { shell.RemoveContainer(context.Background(), container) })
	t.Cleanup(process.Close)

	_, err = process.Start(ctx, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("starting the container: %v", err)
	}

	waitForContainer(t, container, true)

	// Everything this end holds is given up: the context that started the run
	// is cancelled and its result is never waited for. That is exactly the
	// shape of a step whose deadline passed.
	cancel()

	// This is the bug in one assertion: this end is gone and the container is
	// not. Nothing but the name can reclaim it now.
	if !containerRunning(t, container) {
		t.Fatal("container stopped when its caller gave up; this test can no longer detect stranding")
	}

	shell.RemoveContainer(context.Background(), container)

	if containerRunning(t, container) {
		t.Error("container still running after RemoveContainer; a timed-out step would strand it")
	}
}

// TestCLIContainerIntegrationProbeImage exercises the preflight probe against
// a real image: a binary the image has, and one it does not.
func TestCLIContainerIntegrationProbeImage(t *testing.T) {
	requireDockerAgent(t)

	prepared, _ := containerized(t)

	// The probe asks `<binary> --version`, so the stand-in has to be a binary
	// that answers it — most of busybox's applets do not, which is a property
	// of this test image rather than of the probe (a real CLI image has a
	// claude that does).
	err := probeCLIImage(t.Context(), prepared.ri, "apk", 60*time.Second)
	if err != nil {
		t.Errorf("probing an image that has the binary: %v", err)
	}

	err = probeCLIImage(t.Context(), prepared.ri, "definitely-not-installed", 60*time.Second)
	if err == nil {
		t.Fatal("probing an image WITHOUT the binary succeeded, want an error")
	}

	if !strings.Contains(err.Error(), "definitely-not-installed") {
		t.Errorf("error %q does not name the missing binary", err)
	}
}

// TestCLIContainerIntegrationProbeImageDoesNotPull pins --pull=never: the
// probe must fail on an absent image rather than downloading it inside its
// own timeout, which is what turns a slow network into "the image cannot run
// the cli".
func TestCLIContainerIntegrationProbeImageDoesNotPull(t *testing.T) {
	requireDockerAgent(t)

	prepared, _ := containerized(t)
	prepared.ri.Image = "steps-does-not-exist-locally:no-such-tag"

	err := probeCLIImage(t.Context(), prepared.ri, "sh", 60*time.Second)
	if err == nil {
		t.Fatal("probing an absent image succeeded, want an error")
	}

	// Docker's own words for a --pull=never miss, and the point of the
	// assertion: the daemon refused locally. Without the flag this would
	// instead be a registry error after a download attempt — or, for an image
	// that DOES exist remotely, a long pull charged to the probe's timeout.
	if !strings.Contains(err.Error(), "No such image") {
		t.Errorf("error %q does not look like a local --pull=never miss", err)
	}
}

// containerRunning reports whether a container by that name is up.
func containerRunning(t *testing.T, name string) bool {
	t.Helper()

	//nolint:gosec // name is a hex string shell.NewContainerName generated
	out, err := exec.CommandContext(context.Background(), "docker", "inspect",
		"-f", "{{.State.Running}}", name).Output()
	if err != nil {
		return false // no such container
	}

	return strings.TrimSpace(string(out)) == "true"
}

// waitForContainer waits until a container reaches the wanted running state,
// so a test does not race the daemon's own startup.
func waitForContainer(t *testing.T, name string, want bool) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if containerRunning(t, name) == want {
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("container %s did not reach running=%v in time", name, want)
}
