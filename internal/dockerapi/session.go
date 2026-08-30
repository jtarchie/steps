package dockerapi

// Starting a container and running commands in it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ContainerSpec is everything that decides how a container is configured.
//
// Values, not flags. The distinction is worth naming because it removes a
// class of problem rather than guarding against it: there is no argument
// vector for an image name to be misread as an option in, and no `-e NAME`
// forwarding trick needed to keep a secret out of one, since the environment
// travels in a request body that no process list can show.
type ContainerSpec struct {
	Image string
	// Cmd is the container's command, passed as an argv. It is subject to the
	// image's own ENTRYPOINT exactly as `docker run` would be — deliberately,
	// because an image that swallows the command is a real misconfiguration
	// and overriding the entrypoint here would hide it.
	Cmd  []string
	Name string
	// WorkingDir is also the bind mount's path on both sides; empty mounts
	// nothing and takes the image's own workdir.
	WorkingDir string
	// Env are already-resolved NAME=value pairs. A variable the caller could
	// not resolve is simply absent from this, which is what makes naming an
	// optional variable safe.
	Env         []string
	Labels      map[string]string
	User        string
	Network     string
	Privileged  bool
	CPUShares   int64
	MemoryBytes int64
	// Init supplies a real PID 1. It matters for a session container more
	// than for a one-shot: processes an exec leaves behind reparent to PID 1,
	// and a keepalive that is a bare `sleep` would never reap them.
	Init bool
	// AutoRemove has the daemon delete the container when it exits. Right for
	// a foreground run whose lifetime IS the process's; wrong for a session
	// container, whose postmortem is the whole diagnosis when an image
	// rejects the keepalive.
	AutoRemove bool
	// ExtraHosts are `name:address` entries, for reaching the host from
	// inside by a name that works on Docker Desktop and Linux alike.
	ExtraHosts []string
	// Mounts are bind mounts beyond WorkingDir, as `source:target[:ro]`.
	Mounts []string
	// OpenStdin keeps the container's stdin open for a caller that attaches
	// to it, which a foreground run does and a session container does not.
	OpenStdin bool
}

// CreateContainer defines a container without starting it, returning its id.
//
// An image the daemon does not have fails HERE, naming the image, rather than
// later as a container that would not start.
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	config := &container.Config{
		Image:      spec.Image,
		Cmd:        spec.Cmd,
		Env:        spec.Env,
		Labels:     spec.Labels,
		User:       spec.User,
		WorkingDir: spec.WorkingDir,
		OpenStdin:  spec.OpenStdin,
		StdinOnce:  spec.OpenStdin,
	}

	hostConfig := &container.HostConfig{
		Binds:       spec.binds(),
		NetworkMode: container.NetworkMode(spec.Network),
		Privileged:  spec.Privileged,
		AutoRemove:  spec.AutoRemove,
		ExtraHosts:  spec.ExtraHosts,
		Resources: container.Resources{
			// Zero omits each rather than passing 0, which the daemon reads
			// as "no limit" — the same thing, but spelled in a way that makes
			// a misconfiguration look deliberate in an inspect.
			CPUShares: spec.CPUShares,
			Memory:    spec.MemoryBytes,
		},
	}

	if spec.Init {
		enabled := true
		hostConfig.Init = &enabled
	}

	created, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       spec.Name,
		Config:     config,
		HostConfig: hostConfig,
	})
	if err != nil {
		return "", fmt.Errorf("creating a container from image %q: %w", spec.Image, err)
	}

	for _, warning := range created.Warnings {
		slog.Warn("dockerapi.container_create_warning", "image", spec.Image, "warning", warning)
	}

	return created.ID, nil
}

// binds is the working directory plus whatever else the caller asked for.
//
// The working directory is mounted at its own path on both sides so host-side
// readers of the same tree stay coherent with what a containerized command
// wrote.
func (spec ContainerSpec) binds() []string {
	if spec.WorkingDir == "" {
		return spec.Mounts
	}

	binds := make([]string, 0, len(spec.Mounts)+1)
	binds = append(binds, spec.WorkingDir+":"+spec.WorkingDir)

	return append(binds, spec.Mounts...)
}

// StartContainer runs a created container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	_, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	if err != nil {
		return fmt.Errorf("starting container %s: %w", id, err)
	}

	return nil
}

// SettleFor waits up to bound for a container to die, reporting whether it
// did and with what status.
//
// This is the question "did it die AT BIRTH", and it cannot be answered by
// looking: starting a container reports that it STARTED, and an image whose
// entrypoint swallows the keepalive is still running at the instant the start
// returns — it exits a few milliseconds later. Inspecting immediately says
// "running" and is simply wrong, after which every exec reports a container
// that does not exist, naming neither the image nor the reason.
//
// The bound is a real cost: a healthy container pays it once per step. It is
// paid deliberately, and it is not new — the shape this replaced spawned a
// whole `docker inspect` process to ask the same question, which cost more.
func (c *Client) SettleFor(ctx context.Context, id string, bound time.Duration) (died bool, exitCode int, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	result := c.api.ContainerWait(waitCtx, id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})

	select {
	case status := <-result.Result:
		return true, int(status.StatusCode), nil
	case waitErr := <-result.Error:
		// The bound elapsing is the ANSWER — the container is still up — and
		// arrives here as the wait's context ending. Anything else is a
		// daemon that could not be asked, which is not a reason to fail a
		// container that may well be fine.
		if waitCtx.Err() != nil && ctx.Err() == nil {
			return false, 0, nil
		}

		return false, 0, fmt.Errorf("waiting on container %s: %w", id, waitErr)
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return false, 0, fmt.Errorf("waiting on container %s: %w", id, ctx.Err())
		}

		return false, 0, nil
	}
}

// ContainerState reports whether a container is still running, and the status
// it exited with if it is not.
func (c *Client) ContainerState(ctx context.Context, id string) (running bool, exitCode int, err error) {
	inspected, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false, 0, fmt.Errorf("inspecting container %s: %w", id, err)
	}

	if inspected.Container.State == nil {
		return false, 0, fmt.Errorf("container %s: %w", id, errNoState)
	}

	return inspected.Container.State.Running, inspected.Container.State.ExitCode, nil
}

// errNoState is a daemon that described a container without saying what it is
// doing, which nothing here can interpret.
var errNoState = errors.New("the daemon reported no state")

// ContainerLogTail returns the last lines the container produced.
//
// For a container that died at birth this is the image's own error message,
// and it is the whole diagnosis. Best effort by design: a failure to read it
// must not replace the failure being explained.
func (c *Client) ContainerLogTail(ctx context.Context, id string, lines int) string {
	logs, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(lines),
	})
	if err != nil {
		return ""
	}

	defer func() { _ = logs.Close() }()

	var combined strings.Builder

	// Demultiplexed into one buffer on purpose: what is wanted is what the
	// container said, in the order it said it, not which stream it chose.
	_, err = stdcopy.StdCopy(&combined, &combined, logs)
	if err != nil {
		return strings.TrimSpace(combined.String())
	}

	return strings.TrimSpace(combined.String())
}

// ExecOptions is one command to run in an already-running container.
type ExecOptions struct {
	Cmd []string
	// Stdin is fed to the command; nil attaches nothing, so a command that
	// reads stdin sees an immediate end of file rather than waiting.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Exec runs one command in a running container and returns its exit status.
//
// The status is DATA, not an error: a command that exits nonzero has answered,
// and only a failure to run it at all is reported as an error here. That
// distinction is what the whole pipeline uses to tell a step saying no from
// the machinery breaking, and collapsing the two would classify every red
// step as infrastructure.
func (c *Client) Exec(ctx context.Context, containerID string, opts ExecOptions) (int, error) {
	created, err := c.api.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          opts.Cmd,
		AttachStdin:  opts.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return 0, fmt.Errorf("preparing a command in container %s: %w", containerID, err)
	}

	attached, err := c.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return 0, fmt.Errorf("attaching to a command in container %s: %w", containerID, err)
	}

	defer attached.Close()

	// A cancelled caller has to stop this read, and nothing else can: the
	// output arrives on a hijacked connection, so StdCopy blocks in a socket
	// read that no context reaches. Closing the connection is the only
	// interruption there is. The command itself keeps running in the
	// container — exactly as killing a `docker exec` client did — and is
	// bounded by the session's own teardown.
	stopWatching := watchForCancel(ctx, attached.Conn)
	defer stopWatching()

	if opts.Stdin != nil {
		feedStdin(&attached, opts.Stdin)
	}

	// Demultiplexed: the daemon carries both streams over one connection with
	// a frame header, so a plain copy would interleave stderr into the stdout
	// a resource step parses.
	_, copyErr := stdcopy.StdCopy(writerOrDiscard(opts.Stdout), writerOrDiscard(opts.Stderr), attached.Reader)

	// Answered BEFORE the status is asked for, and the order is the bug this
	// had. A copy that ended early because the CALLER gave up is not a failure
	// to run the command: it ran, and this end stopped listening. Asking the
	// daemon first meant asking on a context that had just died, so the
	// inspect failed and its error was reported instead — a timed-out step
	// came back as "docker failed to start", classified as infrastructure
	// rather than as the step being cut off, and threw away every byte it had
	// captured before the deadline.
	if copyErr != nil && ctx.Err() != nil {
		return SignalledExitCode, nil
	}

	// Stripped of cancellation for the narrow window that is left: the output
	// is already complete, so a context dying between the last byte and this
	// question would lose a status the daemon is holding. Not covered by a
	// test — the cancel branch above catches every case a test can arrange,
	// and this is the gap between two statements — so it is written to be
	// obviously right rather than proven.
	inspected, err := c.api.ExecInspect(context.WithoutCancel(ctx), created.ID, client.ExecInspectOptions{})
	if err != nil {
		if copyErr != nil {
			return 0, fmt.Errorf("reading the command's output: %w", copyErr)
		}

		return 0, fmt.Errorf("reading the command's status: %w", err)
	}

	if copyErr != nil {
		return 0, fmt.Errorf("reading the command's output: %w", copyErr)
	}

	return inspected.ExitCode, nil
}

// SignalledExitCode is the status reported for a command that started and was
// then cut off rather than choosing its own — the same sentinel os/exec gives
// for a locally signalled process.
const SignalledExitCode = -1

// feedStdin writes the caller's input to the command and closes the write
// side, which is what tells a command reading stdin that there is no more.
//
// Only called when there IS input. A command with none is not given an
// attached stdin at all, so it reads an immediate end of file — there is no
// half-open stream to close, which is why nothing here handles that case.
//
// ponytail: with an INTERACTIVE terminal as stdin the goroutine parks in a
// read that nothing local can interrupt, and only notices the command is over
// on the next keystroke. Piped or redirected input — the case a pipeline
// actually has — ends on its own. The upgrade path is a stdin reader the
// caller can close.
func feedStdin(attached *client.ExecAttachResult, stdin io.Reader) {
	go func() {
		_, _ = io.Copy(attached.Conn, stdin)
		_ = attached.CloseWrite()
	}()
}

// watchForCancel closes conn if ctx ends first, returning a function that
// stops watching. The returned function must be called, or the watcher
// outlives the exec it was guarding.
func watchForCancel(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

// writerOrDiscard lets a caller ask for only one of the two streams.
func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}

	return w
}
