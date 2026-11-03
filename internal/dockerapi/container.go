package dockerapi

// Finding, running and removing containers.

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Container is what a caller needs in order to decide whether a container is
// theirs and to act on it. Deliberately not the API's own type: this package
// exists so that the shape of an engine response stops at its boundary.
type Container struct {
	ID     string
	Labels map[string]string
}

// ListContainers returns every container — running or not — carrying all of
// the given labels.
//
// All of them, not any: the filter is what keeps a sweep from removing a
// container belonging to another machine or another live process, which is
// the one mistake in this area that breaks a working build rather than merely
// failing to tidy up after a broken one.
//
// Stopped containers are included because they are the orphans most worth
// reclaiming: a steps process killed outright leaves a container whose
// keepalive eventually expires, and it then sits as an inert Exited row that
// nothing else will ever remove.
func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]Container, error) {
	filters := client.Filters{}
	for key, value := range labels {
		filters = filters.Add("label", key+"="+value)
	}

	result, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("listing containers on %s: %w", c.host, err)
	}

	containers := make([]Container, 0, len(result.Items))
	for _, item := range result.Items {
		containers = append(containers, Container{ID: item.ID, Labels: item.Labels})
	}

	return containers, nil
}

// RemoveContainer deletes a container by id or name, stopping it first.
//
// A container that is already gone is a SUCCESS, not a failure. It happens
// constantly — one that exited on its own, a daemon that restarted, a
// teardown racing the sweep — and reporting it would put an error in the log
// of a run that cleaned up perfectly well. This used to be decided by matching
// the words "No such container" in the CLI's output, a sentence the daemon was
// free to reword at any time; the daemon now says which kind of failure it is.
//
// So is a container the daemon is ALREADY removing, which it reports as a
// conflict rather than as a missing container. That is the ordinary outcome
// for a self-removing container whose caller also reclaims it by name — the
// belt and the braces arriving together — and calling it a failure fills the
// log of a perfectly clean run with warnings about the cleanup working twice.
//
// RemoveVolumes takes the container's anonymous volumes with it (an image's VOLUME, like postgres's PGDATA); named volumes are never touched.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !errdefs.IsNotFound(err) && !errdefs.IsConflict(err) {
		return fmt.Errorf("removing container %s: %w", id, err)
	}

	return nil
}

// RunOnce runs spec to completion and removes it: its exit code, and stdout kept apart from stderr so a one-line answer can be read as one.
func (c *Client) RunOnce(ctx context.Context, spec ContainerSpec) (int, string, string, error) {
	id, err := c.CreateContainer(ctx, spec)
	if err != nil {
		return 0, "", "", err
	}

	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalBound)
		defer cancel()

		_ = c.RemoveContainer(removeCtx, id)
	}()

	err = c.StartContainer(ctx, id)
	if err != nil {
		return 0, "", "", err
	}

	// Not-running is safe here, unlike on a container never started: it answers with the real exit even if the container already finished.
	result := c.api.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})

	var code int

	select {
	case status := <-result.Result:
		code = int(status.StatusCode)
	case err = <-result.Error:
		return 0, "", "", fmt.Errorf("waiting on container %s: %w", id, err)
	}

	logs, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return code, "", "", fmt.Errorf("reading container %s's output: %w", id, err)
	}
	defer func() { _ = logs.Close() }()

	stdout, stderr := &cappedBuffer{}, &cappedBuffer{}

	_, err = stdcopy.StdCopy(stdout, stderr, logs)
	if err != nil {
		return code, "", "", fmt.Errorf("reading container %s's output: %w", id, err)
	}

	return code, strings.TrimSpace(stdout.String()), stderr.String(), nil
}

// runOnceOutputCap bounds each of RunOnce's streams: the output is whatever the image chooses to print, and the one caller reads a digest from the first line, so truncating (with a marker) loses nothing it parses.
const runOnceOutputCap = 1 << 20

// cappedBuffer keeps the first runOnceOutputCap bytes and swallows the rest, reporting full writes so the log stream is still drained rather than failed with a short write.
type cappedBuffer struct {
	buf       strings.Builder
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)

	room := runOnceOutputCap - b.buf.Len()
	if len(p) > room {
		p, b.truncated = p[:room], true
	}

	_, _ = b.buf.Write(p)

	return n, nil
}

func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[output truncated at 1 MiB]"
	}

	return b.buf.String()
}
