package dockerapi

// Long-lived containers a pipeline keeps up (the built-in docker-service type): a network to put them on, and what one is running now.

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ServiceLabel names, on a docker-service container, the container name its resource gave it; ServiceSpecLabel is the hash of the configuration it was created from, which is how a put knows whether to replace it.
const (
	ServiceLabel     = "steps.service"
	ServiceSpecLabel = "steps.service.spec"
)

// ServiceState is what a service container is now.
type ServiceState struct {
	ID      string
	Running bool
	// Spec is the ServiceSpecLabel it was created with, "" for a container steps did not make.
	Spec string
}

// InspectService reports the container named name, and false when there is none.
func (c *Client) InspectService(ctx context.Context, name string) (ServiceState, bool, error) {
	result, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return ServiceState{}, false, nil
	}

	if err != nil {
		return ServiceState{}, false, fmt.Errorf("inspecting container %s on %s: %w", name, c.host, err)
	}

	inspected := result.Container
	state := ServiceState{ID: inspected.ID}

	if inspected.State != nil {
		state.Running = inspected.State.Running
	}

	if inspected.Config != nil {
		state.Spec = inspected.Config.Labels[ServiceSpecLabel]
	}

	return state, true, nil
}

// EnsureNetwork creates the bridge network name unless it is already there.
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	_, err := c.api.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}

	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspecting network %s on %s: %w", name, c.host, err)
	}

	_, err = c.api.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: map[string]string{"steps.owner": "steps"}})
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("creating network %s on %s: %w", name, c.host, err)
	}

	return nil
}

// RemoveNetwork deletes a network; one already gone is a success, as for RemoveContainer.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	_, err := c.api.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing network %s: %w", name, err)
	}

	return nil
}
