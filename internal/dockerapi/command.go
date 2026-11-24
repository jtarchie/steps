package dockerapi

// A command run as a container's main process, so the runtime keeps its output and exit code for as long as the container exists (steps#306): what an exec cannot offer once nobody is reading it.

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// JSONFileLogs is the log driver a command container is created with. Chosen here, never left to the worker: Podman's default journald drops lines under load and never ends a follow on an exited container, and a resume point counted in bytes needs a log that reads back byte-exact.
const JSONFileLogs = "json-file"

// StartWithInput starts a created container, writing input to its stdin first when there is any. The container must have been created with OpenStdin; the stream is attached before the start so no byte is read before it is there.
func (c *Client) StartWithInput(ctx context.Context, id string, input io.Reader) error {
	if input == nil {
		return c.StartContainer(ctx, id)
	}

	attached, err := c.api.ContainerAttach(ctx, id, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		return fmt.Errorf("attaching to container %s's input: %w", id, err)
	}
	defer attached.Close()

	err = c.StartContainer(ctx, id)
	if err != nil {
		return err
	}

	_, err = io.Copy(attached.Conn, input)
	if err != nil {
		return fmt.Errorf("writing container %s's input: %w", id, err)
	}

	err = attached.CloseWrite()
	if err != nil {
		return fmt.Errorf("closing container %s's input: %w", id, err)
	}

	return nil
}

// Logs copies a container's recorded output, demultiplexed. With follow it keeps reading until the container stops or the connection does; without, it reads what is recorded and returns. Follow decides nothing about completeness: Podman ends one early on an exited container, so a caller reads once more without it.
func (c *Client) Logs(ctx context.Context, id string, follow bool, stdout, stderr io.Writer) error {
	logs, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow})
	if err != nil {
		return fmt.Errorf("reading container %s's output: %w", id, err)
	}
	defer func() { _ = logs.Close() }()

	_, err = stdcopy.StdCopy(writerOrDiscard(stdout), writerOrDiscard(stderr), logs)
	if err != nil {
		return fmt.Errorf("reading container %s's output: %w", id, err)
	}

	return nil
}

// Wait blocks until the container is not running and returns its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	result := c.api.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})

	select {
	case status := <-result.Result:
		if status.Error != nil && status.Error.Message != "" {
			return 0, fmt.Errorf("waiting on container %s: %s", id, status.Error.Message) //nolint:err113 // the daemon's own words
		}

		return int(status.StatusCode), nil
	case err := <-result.Error:
		return 0, fmt.Errorf("waiting on container %s: %w", id, err)
	}
}

// Kill sends signal to a container's main process. One already stopped or gone is the outcome wanted, not a failure.
func (c *Client) Kill(ctx context.Context, id, signal string) error {
	_, err := c.api.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: signal})
	if err != nil && !errdefs.IsNotFound(err) && !errdefs.IsConflict(err) {
		return fmt.Errorf("signalling container %s: %w", id, err)
	}

	return nil
}

// ContainerStatus is what the daemon says a container is doing (created, running, exited, and so on) and the code it exited with; a missing container satisfies IsNotFound.
func (c *Client) ContainerStatus(ctx context.Context, id string) (string, int, error) {
	inspected, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("inspecting container %s: %w", id, err)
	}

	if inspected.Container.State == nil {
		return "", 0, fmt.Errorf("container %s: %w", id, errNoState)
	}

	return string(inspected.Container.State.Status), inspected.Container.State.ExitCode, nil
}

// ContainerLabels is a container's labels; a missing container satisfies IsNotFound.
func (c *Client) ContainerLabels(ctx context.Context, id string) (map[string]string, error) {
	inspected, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", id, err)
	}

	if inspected.Container.Config == nil {
		return map[string]string{}, nil
	}

	return inspected.Container.Config.Labels, nil
}
