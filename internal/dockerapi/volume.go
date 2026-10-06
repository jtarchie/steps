package dockerapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	volumetypes "github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// Volume is a daemon-side directory; Mountpoint is its path on the DAEMON's disk, which is what an overlay's options must name.
type Volume struct {
	Name       string
	Mountpoint string
	Labels     map[string]string
	// CreatedAt is zero when the daemon did not say or said something unparseable.
	CreatedAt time.Time
}

// NewDialer reaches a daemon through dial, for a socket with no address of its own here (one forwarded over ssh); name is what errors call it.
func NewDialer(name string, dial func(ctx context.Context) (net.Conn, error)) (*Client, error) {
	api, err := client.New(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to the docker daemon at %s: %w", name, err)
	}

	return &Client{api: api, host: name}, nil
}

var errNoLabels = errors.New("no labels to select by")

func volumeOf(v volumetypes.Volume) Volume {
	created, _ := time.Parse(time.RFC3339Nano, v.CreatedAt)

	return Volume{Name: v.Name, Mountpoint: v.Mountpoint, Labels: v.Labels, CreatedAt: created}
}

// IsNotFound reports a daemon answering that the thing asked about does not exist.
func IsNotFound(err error) bool { return errdefs.IsNotFound(err) }

// OverlayOptions are the local driver's options for a copy-on-write child of lower; all three are daemon-side paths, and upper and work must share a filesystem.
// ponytail: paths are joined unescaped, safe while they are daemon Mountpoints; validate ',' and ':' if a caller ever passes its own.
func OverlayOptions(lower, upper, work string) map[string]string {
	return map[string]string{
		"type":   "overlay",
		"device": "overlay",
		"o":      "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work,
	}
}

// CreateVolume makes a local-driver volume, or returns an existing one of that name untouched, old contents and old options alike (the daemon's answer, not an error); options are checked only at first mount.
func (c *Client) CreateVolume(ctx context.Context, name string, labels, driverOpts map[string]string) (Volume, error) {
	created, err := c.api.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Driver: "local", DriverOpts: driverOpts, Labels: labels})
	if err != nil {
		return Volume{}, fmt.Errorf("creating volume %s on %s: %w", name, c.host, err)
	}

	return volumeOf(created.Volume), nil
}

// InspectVolume errors with IsNotFound for a volume the daemon does not have.
func (c *Client) InspectVolume(ctx context.Context, name string) (Volume, error) {
	inspected, err := c.api.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return Volume{}, fmt.Errorf("inspecting volume %s on %s: %w", name, c.host, err)
	}

	return volumeOf(inspected.Volume), nil
}

// ListVolumes returns the volumes carrying every one of labels; no labels is refused, since a sweep over the result would reach every volume on the daemon.
func (c *Client) ListVolumes(ctx context.Context, labels map[string]string) ([]Volume, error) {
	if len(labels) == 0 {
		return nil, fmt.Errorf("listing volumes on %s: %w", c.host, errNoLabels)
	}

	filters := client.Filters{}
	for key, value := range labels {
		filters = filters.Add("label", key+"="+value)
	}

	listed, err := c.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("listing volumes on %s: %w", c.host, err)
	}

	volumes := make([]Volume, 0, len(listed.Items))
	for _, item := range listed.Items {
		volumes = append(volumes, volumeOf(item))
	}

	return volumes, nil
}

// RemoveVolume treats an absent volume as removed, but never forces one in use: a lower pulled from under a mounted overlay is undefined behaviour.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.api.VolumeRemove(ctx, name, client.VolumeRemoveOptions{})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing volume %s on %s: %w", name, c.host, err)
	}

	return nil
}

// CreateHolder creates a never-started container whose only job is to mount volumes for the archive endpoints; image must already be present.
func (c *Client) CreateHolder(ctx context.Context, name, image string, labels map[string]string, mounts []string) (string, error) {
	created, err := c.createContainer(ctx, client.ContainerCreateOptions{
		Name: name,
		// An image only because the client refuses a create without one (the daemon would not); never started, so never run.
		Config:     &container.Config{Image: image, Cmd: []string{"true"}, Labels: labels},
		HostConfig: &container.HostConfig{Binds: mounts},
	})
	if err != nil {
		return "", fmt.Errorf("creating holder %s on %s: %w", name, c.host, err)
	}

	return created.ID, nil
}

// PutArchive unpacks a tar stream at dst inside the container, which lands in whatever volume is mounted there.
func (c *Client) PutArchive(ctx context.Context, id, dst string, content io.Reader) error {
	_, err := c.api.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: dst, Content: content})
	if err != nil {
		return fmt.Errorf("copying into %s:%s on %s: %w", id, dst, c.host, err)
	}

	return nil
}

// GetArchive streams src out of the container as tar; the caller closes it.
func (c *Client) GetArchive(ctx context.Context, id, src string) (io.ReadCloser, error) {
	copied, err := c.api.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: src})
	if err != nil {
		return nil, fmt.Errorf("copying %s:%s out of %s: %w", id, src, c.host, err)
	}

	return copied.Content, nil
}

// RunOnce runs spec to completion and removes it: its exit code, and stdout kept apart from stderr so a one-line answer can be read as one.
func (c *Client) RunOnce(ctx context.Context, spec ContainerSpec) (int, string, string, error) {
	id, err := c.CreateContainer(ctx, spec)
	if err != nil {
		return 0, "", "", err
	}

	defer func() { _ = c.RemoveContainer(context.WithoutCancel(ctx), id) }()

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

	var stdout, stderr strings.Builder

	_, err = stdcopy.StdCopy(&stdout, &stderr, logs)
	if err != nil {
		return code, "", "", fmt.Errorf("reading container %s's output: %w", id, err)
	}

	return code, strings.TrimSpace(stdout.String()), stderr.String(), nil
}
