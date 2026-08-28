package shell

// A container step aimed at a daemon that is not this process's default, with
// a mount path this machine did not resolve.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// currentDockerHost is the daemon this machine talks to, in DOCKER_HOST form.
func currentDockerHost(t *testing.T) string {
	t.Helper()

	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Skipf("cannot determine the docker host: %v", err)
	}

	return strings.TrimSpace(string(out))
}

// TestRunnerTargetsANamedDaemon pins that DockerHost reaches the invocation.
//
// Aimed at this machine's own daemon by its explicit address rather than by
// default, which is the only part a test without a second daemon can prove —
// and the part that matters, since a venue's forwarded socket is just another
// address in that same field.
func TestRunnerTargetsANamedDaemon(t *testing.T) {
	requireDocker(t)

	dir := mountableTempDir(t)

	err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the input: %v", err)
	}

	runner, err := NewRunner(RunnerSpec{
		Image:      testImage,
		Cwd:        dir,
		DockerHost: currentDockerHost(t),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	out, err := runner.RunCapture(context.Background(), "cat seed.txt")
	if err != nil {
		t.Fatalf("RunCapture: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "seed") {
		t.Errorf("output = %s, want the mounted tree readable through the named daemon", out)
	}

	// The half that can actually fail. Aiming at this machine's own daemon
	// proves nothing on its own — it is also the default, so a runner that
	// ignored DockerHost entirely would pass the assertion above. A daemon
	// that does not exist is the only address a test without a second daemon
	// can distinguish, and a runner still talking to the default succeeds
	// here instead of failing.
	absent, err := NewRunner(RunnerSpec{
		Image:      testImage,
		Cwd:        dir,
		DockerHost: "unix://" + filepath.Join(t.TempDir(), "no-daemon.sock"),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = absent.Close() })

	_, err = absent.RunCapture(context.Background(), "true")
	if err == nil {
		t.Error("a runner aimed at a daemon that does not exist succeeded — DockerHost was ignored")
	}
}

// TestRunnerMountsAPathItDidNotResolve pins the seam a placed containerized
// step turns on: the bind mount names a path on the DAEMON's filesystem, and
// this process must not try to resolve it.
//
// Proven by handing MountPath a directory that exists — so the container can
// read it — while pointing Cwd at one that does not. ResolveMountPath would
// fail on that Cwd, so a runner that still resolved locally cannot reach the
// assertion at all.
func TestRunnerMountsAPathItDidNotResolve(t *testing.T) {
	requireDocker(t)

	dir := mountableTempDir(t)

	err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o600)
	if err != nil {
		t.Fatalf("writing the input: %v", err)
	}

	resolved, err := ResolveMountPath(dir)
	if err != nil {
		t.Fatalf("ResolveMountPath: %v", err)
	}

	runner, err := NewRunner(RunnerSpec{
		Image:     testImage,
		Cwd:       filepath.Join(dir, "no", "such", "place"),
		MountPath: resolved,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	out, err := runner.RunCapture(context.Background(), "cat seed.txt")
	if err != nil {
		t.Fatalf("RunCapture: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "seed") {
		t.Errorf("output = %s, want the mount path to have been used verbatim", out)
	}
}

// TestSessionStartAimsTheWorkersDaemonOverTheStepsOwnEnv crosses the seam
// where a placed containerized step carries two settings that land on the same
// variable: the daemon the venue forwarded, and the values the pipeline's env:
// resolved on the orchestrator.
//
// Both are written onto the docker client's environment and os/exec keeps the
// LAST duplicate, so a step whose env: names DOCKER_HOST — anything with
// colima, Rancher Desktop, rootless or a remote daemon exported — started its
// container on the ORCHESTRATOR while exec, inspect, logs and rm all still
// went to the worker. The container came up on the wrong machine with
// `-v <worker path>:<worker path>`, which docker silently creates as an empty
// local directory, and every later call reported "No such container".
func TestSessionStartAimsTheWorkersDaemonOverTheStepsOwnEnv(t *testing.T) {
	argvFile := writeFakeDocker(t, 0, "", "")

	const forwarded = "unix:///tmp/steps-worker-forwarded.sock"

	runner, err := NewRunner(RunnerSpec{
		Image:      "alpine",
		MountPath:  "/worker/tree",
		DockerHost: forwarded,
		Env:        []string{"DOCKER_HOST"},
		EnvValues:  map[string]string{"DOCKER_HOST": "unix:///orchestrator.sock"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, _, _, err = runner.RunCaptureFull(context.Background(), "true")
	if err != nil {
		t.Fatalf("RunCaptureFull: %v", err)
	}

	CloseRunner(runner, "test")

	for _, line := range recordedDaemons(t, argvFile) {
		verb, host, _ := strings.Cut(line, " ")
		if host != forwarded {
			t.Errorf("docker %s ran against %q, want the forwarded socket %q", verb, host, forwarded)
		}
	}

	// The start is the invocation that regressed, so it is named rather than
	// left to the loop: a session whose container never started would satisfy
	// that loop without ever having aimed a `docker run` anywhere.
	if runs := invocationsOf(recordedArgv(t, argvFile), "run"); len(runs) != 1 {
		t.Errorf("recorded %v, want exactly one docker run", runs)
	}
}
