package dockerapi

// A container this process drives in the foreground, reading its output as it
// arrives.

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStartAttachedStreamsStdoutAsItArrives(t *testing.T) {
	client := requireDaemon(t)

	attached, err := client.StartAttached(t.Context(), ContainerSpec{
		Image:      testImage,
		Cmd:        []string{"sh", "-c", "echo first; sleep 5; echo second"},
		AutoRemove: true,
	}, nil, io.Discard)
	if err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	t.Cleanup(func() { _, _ = attached.Wait(context.WithoutCancel(t.Context())) })

	// The first line has to be readable while the container is still running.
	// Buffering the whole transcript is the failure this guards: a step that
	// times out mid-conversation would keep nothing of what it managed to do.
	lines := bufio.NewReader(attached.Stdout)

	started := time.Now()

	line, err := lines.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first line: %v", err)
	}

	if strings.TrimSpace(line) != "first" {
		t.Errorf("first line = %q, want %q", strings.TrimSpace(line), "first")
	}

	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the first line took %s to arrive; the output is being buffered rather than streamed", elapsed)
	}
}

func TestStartAttachedSeparatesStderr(t *testing.T) {
	client := requireDaemon(t)

	var stderr strings.Builder

	attached, err := client.StartAttached(t.Context(), ContainerSpec{
		Image:      testImage,
		Cmd:        []string{"sh", "-c", "echo to-stdout; echo to-stderr >&2"},
		AutoRemove: true,
	}, nil, &stderr)
	if err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	stdout, err := io.ReadAll(attached.Stdout)
	if err != nil {
		t.Fatalf("reading stdout: %v", err)
	}

	code, err := attached.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	// The transcript is PARSED, so anything the CLI logs to stderr landing in
	// it would be read as malformed output rather than as a log line.
	if strings.TrimSpace(string(stdout)) != "to-stdout" {
		t.Errorf("stdout = %q, want only the stdout line", stdout)
	}

	if strings.TrimSpace(stderr.String()) != "to-stderr" {
		t.Errorf("stderr = %q, want only the stderr line", stderr.String())
	}
}

func TestStartAttachedCarriesStdinAndReportsExitCode(t *testing.T) {
	client := requireDaemon(t)

	attached, err := client.StartAttached(t.Context(), ContainerSpec{
		Image:      testImage,
		Cmd:        []string{"sh", "-c", "cat; exit 4"},
		AutoRemove: true,
		OpenStdin:  true,
	}, strings.NewReader("the prompt\n"), io.Discard)
	if err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	stdout, err := io.ReadAll(attached.Stdout)
	if err != nil {
		t.Fatalf("reading stdout: %v", err)
	}

	code, err := attached.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if strings.TrimSpace(string(stdout)) != "the prompt" {
		t.Errorf("stdout = %q, want what was written to stdin", stdout)
	}

	// The status is DATA. A CLI that reports a task failure exits nonzero
	// while still having spoken for itself, and a caller that treated the
	// status as the verdict would call every such run an infrastructure
	// error.
	if code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// TestStartAttachedStdoutEndsWhenTheContainerDoes pins that a reader of the
// transcript sees an end of file rather than waiting forever.
func TestStartAttachedStdoutEndsWhenTheContainerDoes(t *testing.T) {
	client := requireDaemon(t)

	attached, err := client.StartAttached(t.Context(), ContainerSpec{
		Image:      testImage,
		Cmd:        []string{"sh", "-c", "echo done"},
		AutoRemove: true,
	}, nil, io.Discard)
	if err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = io.Copy(io.Discard, attached.Stdout)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the transcript stream never ended; a caller reading it would hang until the step's timeout")
	}

	_, err = attached.Wait(t.Context())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestStartAttachedNamesTheContainerSoItCanBeReclaimed pins the property the
// whole named-container arrangement exists for: killing this end does not stop
// the container, so its caller must be able to take it away by name.
func TestStartAttachedNamesTheContainerSoItCanBeReclaimed(t *testing.T) {
	client := requireDaemon(t)

	const name = "steps-test-attached-reclaim"

	_ = client.RemoveContainer(t.Context(), name)

	attached, err := client.StartAttached(t.Context(), ContainerSpec{
		Image: testImage,
		Name:  name,
		Cmd:   []string{"sh", "-c", "sleep 120"},
	}, nil, io.Discard)
	if err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	t.Cleanup(func() { _ = client.RemoveContainer(context.WithoutCancel(t.Context()), name) })

	err = client.RemoveContainer(t.Context(), name)
	if err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}

	// And removing it ends the stream, which is what lets a timed-out caller
	// stop reading rather than waiting out a container it has already killed.
	_, err = io.Copy(io.Discard, attached.Stdout)
	if err != nil && !strings.Contains(err.Error(), "closed") {
		t.Errorf("reading after removal: %v", err)
	}
}

// TestWaitDoesNotDeadlockWhenTheCallerStoppedReading pins the hazard that a
// streamed transcript creates and a collected one does not.
//
// A caller reads the container's output as it arrives, which means it can stop
// early: a parse that gives up on an over-long line, a step whose deadline
// passes. The demultiplexer is then blocked writing into a pipe nobody drains,
// and a Wait that simply waited for it would hang against a caller who has
// done nothing wrong — for as long as the container keeps talking, which for
// the agent CLI this exists for is the whole conversation.
func TestWaitDoesNotDeadlockWhenTheCallerStoppedReading(t *testing.T) {
	client := requireDaemon(t)

	attached, err := client.StartAttached(t.Context(), ContainerSpec{
		Image: testImage,
		// Far more than any pipe will hold, so the writer is certainly blocked
		// by the time Wait is called.
		Cmd:        []string{"sh", "-c", "i=0; while [ $i -lt 20000 ]; do echo line-$i; i=$((i+1)); done"},
		AutoRemove: true,
	}, nil, io.Discard)
	if err != nil {
		t.Fatalf("StartAttached: %v", err)
	}

	// One line, then nothing. This is the caller giving up.
	_, err = bufio.NewReader(attached.Stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first line: %v", err)
	}

	returned := make(chan error, 1)

	go func() {
		_, waitErr := attached.Wait(context.WithoutCancel(t.Context()))
		returned <- waitErr
	}()

	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("Wait never returned; a caller that stopped reading has deadlocked against its own container")
	}
}
