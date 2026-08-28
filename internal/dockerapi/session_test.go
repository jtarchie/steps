package dockerapi

// Starting a container and running commands in it.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// startSession creates and starts a keepalive container for a test, removing
// it afterwards.
func startSession(t *testing.T, client *Client, spec ContainerSpec) string {
	t.Helper()

	if spec.Image == "" {
		spec.Image = testImage
	}

	if len(spec.Cmd) == 0 {
		spec.Cmd = []string{"sh", "-c", "sleep 120"}
	}

	id, err := client.CreateContainer(t.Context(), spec)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	t.Cleanup(func() { _ = client.RemoveContainer(context.WithoutCancel(t.Context()), id) })

	err = client.StartContainer(t.Context(), id)
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	return id
}

func TestExecReportsStreamsAndExitCodeSeparately(t *testing.T) {
	client := requireDaemon(t)
	id := startSession(t, client, ContainerSpec{})

	var stdout, stderr strings.Builder

	code, err := client.Exec(t.Context(), id, ExecOptions{
		Cmd:    []string{"sh", "-c", "echo out; echo err >&2; exit 7"},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}

	// Separately is the whole point: the daemon multiplexes both onto one
	// connection, and a reader that did not demultiplex would hand a resource
	// step's parser whatever the command happened to log.
	if strings.TrimSpace(stdout.String()) != "out" {
		t.Errorf("stdout = %q, want just the stdout line", stdout.String())
	}

	if strings.TrimSpace(stderr.String()) != "err" {
		t.Errorf("stderr = %q, want just the stderr line", stderr.String())
	}
}

// TestExecCarriesStdin pins that a command can be fed. Both runners wire the
// host's stdin through for Run and RunCapture, and a container that quietly
// got /dev/null instead would make `read` hang or answer nothing.
func TestExecCarriesStdin(t *testing.T) {
	client := requireDaemon(t)
	id := startSession(t, client, ContainerSpec{})

	var stdout strings.Builder

	code, err := client.Exec(t.Context(), id, ExecOptions{
		Cmd:    []string{"sh", "-c", "cat"},
		Stdin:  strings.NewReader("fed in\n"),
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	if strings.TrimSpace(stdout.String()) != "fed in" {
		t.Errorf("stdout = %q, want what was written to stdin", stdout.String())
	}
}

// TestExecWithoutStdinDoesNotHang pins the other half: a command that reads
// stdin when nothing is attached must see an immediate end of file rather than
// waiting out the step's timeout.
func TestExecWithoutStdinDoesNotHang(t *testing.T) {
	client := requireDaemon(t)
	id := startSession(t, client, ContainerSpec{})

	var stdout strings.Builder

	code, err := client.Exec(t.Context(), id, ExecOptions{
		Cmd:    []string{"sh", "-c", "cat; echo done"},
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if code != 0 || strings.TrimSpace(stdout.String()) != "done" {
		t.Errorf("exit %d, stdout %q; want a command reading an absent stdin to finish", code, stdout.String())
	}
}

// TestExecStatePersistsBetweenCommands is the behaviour a per-step container
// exists for: an agent's `pip install x` followed by `python y` as two
// separate calls has to work.
func TestExecStatePersistsBetweenCommands(t *testing.T) {
	client := requireDaemon(t)
	id := startSession(t, client, ContainerSpec{})

	_, err := client.Exec(t.Context(), id, ExecOptions{Cmd: []string{"sh", "-c", "echo kept > /tmp/state"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	var stdout strings.Builder

	code, err := client.Exec(t.Context(), id, ExecOptions{
		Cmd:    []string{"sh", "-c", "cat /tmp/state"},
		Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if code != 0 || strings.TrimSpace(stdout.String()) != "kept" {
		t.Errorf("exit %d, stdout %q; want the first command's effect visible to the second", code, stdout.String())
	}
}

// TestContainerStateReportsAContainerThatDiedAtBirth is the diagnosis a
// session start depends on.
//
// Creating and starting a container reports whether it STARTED, not whether it
// is still up: an image whose entrypoint swallows the keepalive exits a
// millisecond later and reports success. Taking that at face value left every
// later exec saying the container did not exist, which names neither the image
// nor the reason.
func TestContainerStateReportsAContainerThatDiedAtBirth(t *testing.T) {
	client := requireDaemon(t)

	id := startSession(t, client, ContainerSpec{Cmd: []string{"sh", "-c", "echo dying >&2; exit 3"}})

	running, code := waitForContainerExit(t, client, id)

	if running {
		t.Fatal("the container is still running; the fixture was supposed to exit at once")
	}

	if code != 3 {
		t.Errorf("exit code = %d, want the container's own 3", code)
	}

	// The postmortem is the whole diagnosis, and it is gone the moment
	// anything removes the container — which is why a session container is
	// not started with --rm.
	logs := client.ContainerLogTail(t.Context(), id, 10)
	if !strings.Contains(logs, "dying") {
		t.Errorf("log tail = %q, want the container's own last words", logs)
	}
}

func waitForContainerExit(t *testing.T, client *Client, id string) (bool, int) {
	t.Helper()

	for range 50 {
		running, code, err := client.ContainerState(t.Context(), id)
		if err != nil {
			t.Fatalf("ContainerState: %v", err)
		}

		if !running {
			return false, code
		}
	}

	return true, 0
}

// TestCreateContainerReportsAnAbsentImage pins that a bad image fails where it
// can be explained, rather than as a container that will not start.
func TestCreateContainerReportsAnAbsentImage(t *testing.T) {
	client := requireDaemon(t)

	_, err := client.CreateContainer(t.Context(), ContainerSpec{
		Image: "steps-test-no-such-image:definitely-not-here",
		Cmd:   []string{"true"},
	})
	if err == nil {
		t.Fatal("CreateContainer succeeded for an image that does not exist")
	}

	if !strings.Contains(err.Error(), "steps-test-no-such-image") {
		t.Errorf("error = %v, want it to name the image", err)
	}
}

// TestSettleForCatchesAContainerThatDiesInsideTheBound is the question a
// session start actually asks, asked directly.
//
// It is otherwise only reached through internal/shell, one package away, and
// it is the subtlest thing here: the bound ELAPSING is a successful answer
// ("still up") rather than a failure, while the caller's own context ending is
// a real one, and the two arrive on the same channel.
func TestSettleForCatchesAContainerThatDiesInsideTheBound(t *testing.T) {
	client := requireDaemon(t)

	id := startSession(t, client, ContainerSpec{Cmd: []string{"sh", "-c", "exit 5"}})

	died, code, err := client.SettleFor(t.Context(), id, 30*time.Second)
	if err != nil {
		t.Fatalf("SettleFor: %v", err)
	}

	if !died {
		t.Fatal("SettleFor said a container running `exit 5` is still up")
	}

	if code != 5 {
		t.Errorf("exit code = %d, want the container's own 5", code)
	}
}

// TestSettleForLeavesALiveContainerAlone pins the half that runs on every
// healthy step: the bound elapses, and that is the answer rather than an
// error. Reported as a failure, every containerized step would die at its
// own start.
func TestSettleForLeavesALiveContainerAlone(t *testing.T) {
	client := requireDaemon(t)

	id := startSession(t, client, ContainerSpec{})

	started := time.Now()

	died, _, err := client.SettleFor(t.Context(), id, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("SettleFor: %v, want the bound elapsing to be an answer and not a failure", err)
	}

	if died {
		t.Error("SettleFor said a sleeping container had died")
	}

	// And it waited rather than answering instantly, which is the whole
	// mechanism: a container that dies a few milliseconds in is still running
	// at the moment the start returns.
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Errorf("SettleFor returned after %s, want it to have waited out its bound", elapsed)
	}
}

// TestSettleForReportsACancelledCaller pins the case that must NOT be read as
// "still up": the caller gave up, which says nothing about the container.
func TestSettleForReportsACancelledCaller(t *testing.T) {
	client := requireDaemon(t)

	id := startSession(t, client, ContainerSpec{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := client.SettleFor(ctx, id, 30*time.Second)
	if err == nil {
		t.Error("SettleFor reported a healthy container for a caller that had already given up")
	}
}
