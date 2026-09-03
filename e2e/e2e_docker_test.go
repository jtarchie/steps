package e2e

// End-to-end coverage for the one composition neither existing suite reaches:
// an agent step whose commands run in a real container.
//
// The root e2e tests drive agents through the whole stack against a scripted
// fake provider, but always on the host. internal/shell's docker integration
// tests exercise DockerRunner against a real daemon, but never underneath a
// conversation. The seam between them carries real behavior — the model's
// run_shell executing in the container, host-side read_file seeing what it
// wrote through the bind mount, the container persisting across calls, a
// nonzero exit arriving as tool-result data rather than aborting the step —
// and none of it was covered.
//
// Run whenever a daemon is reachable, and skipped only when one is not. They
// were opt-in behind STEPS_TEST_DOCKER, which meant the default sequence was
// green while the feature they cover was broken — the exact way a
// containerized placed step shipped not working.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dockerE2EImage = "alpine:3"

// requireDockerE2E mirrors internal/shell's requireDocker; the root package
// can't reach that one, and duplicating six lines beats exporting a test
// helper from a package that has no other reason to.
func requireDockerE2E(t *testing.T) {
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

	daemonVisibleTemp(t)
}

// daemonVisibleTemp points TMPDIR somewhere the daemon can bind-mount.
//
// A step's tree is mounted into the container BY THE DAEMON, so it has to
// live where the daemon can see it. On macOS the daemon runs in a VM that
// shares the home directory and not /var/folders, where TMPDIR points — and
// docker answers an unshared mount by silently mounting an EMPTY directory,
// so the step succeeds and produces nothing. Every docker-backed e2e here
// builds its workspace under TMPDIR, so this belongs beside the daemon check
// rather than repeated in each test.
func daemonVisibleTemp(t *testing.T) {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to root a daemon-visible workspace in")
	}

	shared, err := os.MkdirTemp(home, ".steps-e2e-*")
	if err != nil {
		t.Skipf("cannot create a daemon-visible temp dir under %s: %v", home, err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(shared) })
	t.Setenv("TMPDIR", shared)
}

// lastToolResult returns the most recent tool result in a captured request.
// capturedRequest.toolResults() returns every tool message accumulated so far
// in the conversation, so indexing from the front reads an earlier call's
// answer — the one thing worth getting right when asserting that call N did
// something.
func lastToolResult(t *testing.T, r capturedRequest) string {
	t.Helper()

	results := r.toolResults()
	if len(results) == 0 {
		t.Fatal("request carried no tool results")
	}

	return results[len(results)-1]
}

// dockerAgentPipeline is an agent whose tools run in a container, working on
// an artifact a get produced.
func dockerAgentPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo seeded > NOTES.txt

resources:
- name: repo
  type: dummy
  source: {}

agents:
- name: inspector
  image: %[2]s
  env: [STEPS_TEST_PASSED_THROUGH]
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

jobs:
- name: inspect
  plan:
  - get: repo
  - agent: inspector
    inputs: [repo]
    messages:
      - Inspect the checked-out repo.
`, endpoint, dockerE2EImage)

	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// TestEndToEndAgentInContainer is the core composition: the model directs a
// shell command, it executes in a container, and the file it wrote through
// the bind mount is readable by the agent's host-side read_file.
func TestEndToEndAgentInContainer(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "echo from-the-container > repo/CONTAINER.txt"}),
		callsTool("read_file", map[string]any{"path": "repo/CONTAINER.txt"}),
		says("the container wrote a file and I read it back"),
	)

	path := dockerAgentPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// The shell tool ran in the container and its exit code came back as data.
	shellResult := lastToolResult(t, fake.request(2))
	if !strings.Contains(shellResult, `"exit_code":0`) {
		t.Errorf("run_shell result = %q, want a zero exit_code from the container", shellResult)
	}

	// read_file runs HOST-side against the bind-mounted directory. Seeing the
	// container's write is what proves both halves address the same path.
	readResult := lastToolResult(t, fake.request(3))
	if !strings.Contains(readResult, "from-the-container") {
		t.Errorf("read_file result = %q, want the content the containerized command wrote", readResult)
	}

	assertSucceeded(t, storeNodes(t, path), "agent", "inspector")
}

// TestEndToEndAgentContainerStatePersistsAcrossToolCalls is issue #48 observed
// from where it actually mattered: two run_shell calls in one conversation.
// Under the old fresh-container-per-command shape the second call ran in a
// container that had never seen the first, which is precisely the pattern a
// model reaches for when asked to install something and then use it.
//
// The marker is written outside the bind mount deliberately — a file under the
// working directory would survive even without a shared container, proving
// nothing.
func TestEndToEndAgentContainerStatePersistsAcrossToolCalls(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "echo persisted > /tmp/marker"}),
		callsTool("run_shell", map[string]any{"command": "cat /tmp/marker"}),
		says("state carried between calls"),
	)

	path := dockerAgentPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	second := lastToolResult(t, fake.request(3))
	if !strings.Contains(second, "persisted") {
		t.Errorf("second run_shell result = %q, want the first call's file — the calls did not share a container", second)
	}
}

// TestEndToEndAgentContainerEnvPassthrough covers issue #50 through the whole
// stack: a container starts from its image's own environment, so a variable
// reaching the command at all is entirely down to env:.
func TestEndToEndAgentContainerEnvPassthrough(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": `echo "[$STEPS_TEST_PASSED_THROUGH][$STEPS_TEST_WITHHELD]"`}),
		says("done"),
	)

	path := dockerAgentPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")
	t.Setenv("STEPS_TEST_PASSED_THROUGH", "visible")
	t.Setenv("STEPS_TEST_WITHHELD", "should-not-appear")

	mustRun(t, path)

	result := lastToolResult(t, fake.request(2))
	if !strings.Contains(result, "[visible]") {
		t.Errorf("run_shell result = %q, want the env: variable to reach the container", result)
	}

	if strings.Contains(result, "should-not-appear") {
		t.Errorf("run_shell result = %q, want a variable NOT named in env: to stay out of the container", result)
	}
}

// TestEndToEndAgentContainerNonzeroExitIsData pins the contract docs/infra.md
// states: a failing command inside a container reaches the model as an
// ordinary tool result it can react to, not as an error that kills the step.
func TestEndToEndAgentContainerNonzeroExitIsData(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "exit 7"}),
		says("the command failed and I carried on"),
	)

	path := dockerAgentPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	result := lastToolResult(t, fake.request(2))
	if !strings.Contains(result, `"exit_code":7`) {
		t.Errorf("run_shell result = %q, want exit_code 7 as data", result)
	}

	assertSucceeded(t, storeNodes(t, path), "agent", "inspector")
}

// TestEndToEndAgentContainerLeavesNothingRunning is issue #48's cleanup half
// end to end: after a whole run finishes, no container this pipeline started
// is still around.
func TestEndToEndAgentContainerLeavesNothingRunning(t *testing.T) {
	requireDockerE2E(t)

	before := runningStepsContainers(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "true"}),
		says("done"),
	)

	path := dockerAgentPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	after := runningStepsContainers(t)
	if len(after) != len(before) {
		t.Errorf("containers named steps-* before = %v, after = %v — the run left one behind", before, after)
	}
}

// runningStepsContainers lists containers this tool would have created. It
// compares before/after rather than asserting emptiness, so a developer with
// unrelated containers around doesn't get a spurious failure.
func runningStepsContainers(t *testing.T) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name=steps-").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}

	return strings.Split(trimmed, "\n")
}
