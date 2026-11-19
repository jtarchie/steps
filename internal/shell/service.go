package shell

// The built-in docker-service type's half that talks to the daemon: keep one container up on a network, replaced only when its configuration changes.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// Service is one docker-service, as the daemon is asked to run it.
type Service struct {
	Container, Image, Network string
	Env                       []string
	// Volumes maps a named volume to the path it is mounted at.
	Volumes map[string]string
	Ready   []string
	Timeout time.Duration
	// Spec is the configuration's hash; a running container with another one is replaced.
	Spec string
}

// errNeverReady is a service whose ready command did not succeed in time.
var errNeverReady = errors.New("never became ready")

// errNotOurs is a container holding the service's name that steps did not create: replacing it would remove the operator's container and its anonymous volumes.
var errNotOurs = errors.New("steps did not create it")

// EnsureService makes the service's network, replaces its container when the configuration changed (or starts it when it is merely stopped), and returns once its ready command succeeds.
func EnsureService(ctx context.Context, service Service, progress io.Writer) error {
	client, err := dockerapi.New("")
	if err != nil {
		return fmt.Errorf("service %s: %w", service.Container, err)
	}

	defer func() { _ = client.Close() }()

	err = client.EnsureNetwork(ctx, service.Network)
	if err != nil {
		return fmt.Errorf("service %s: %w", service.Container, err)
	}

	id, err := runService(ctx, client, service, progress)
	if err != nil {
		return fmt.Errorf("service %s: %w", service.Container, err)
	}

	return waitReady(ctx, client, id, service)
}

// runService leaves the service's container running and returns its id.
func runService(ctx context.Context, client *dockerapi.Client, service Service, progress io.Writer) (string, error) {
	state, found, err := client.InspectService(ctx, service.Container)
	if err != nil {
		return "", err //nolint:wrapcheck // the caller names the service
	}

	if found && state.Spec == "" {
		return "", notOurs(service.Container)
	}

	if found && state.Spec == service.Spec {
		if !state.Running {
			err = client.StartContainer(ctx, state.ID)
		}

		return state.ID, err //nolint:wrapcheck // the caller names the service
	}

	if found {
		slog.InfoContext(ctx, "shell.service.replace", "container", service.Container, "was", state.Spec, "now", service.Spec)

		err = client.RemoveContainer(ctx, state.ID)
		if err != nil {
			return "", err //nolint:wrapcheck // the caller names the service
		}
	}

	return createService(ctx, client, service, progress)
}

// createService pulls the image if it is not here, and creates and starts the container.
func createService(ctx context.Context, client *dockerapi.Client, service Service, progress io.Writer) (string, error) {
	if !client.ImagePresent(ctx, service.Image) {
		err := client.Pull(ctx, service.Image, progress)
		if err != nil {
			return "", err //nolint:wrapcheck // the caller names the service
		}
	}

	volumes := make([]dockerapi.Bind, 0, len(service.Volumes))
	for volume, path := range service.Volumes {
		volumes = append(volumes, dockerapi.Bind{Source: volume, Target: path})
	}

	// No steps.owner or steps.pid label: those mark what a sweep may reclaim, and a service is meant to outlive the run that made it.
	id, err := client.CreateContainer(ctx, dockerapi.ContainerSpec{
		Image:         service.Image,
		Name:          service.Container,
		Env:           service.Env,
		Network:       service.Network,
		Volumes:       volumes,
		RestartAlways: true,
		Labels:        map[string]string{dockerapi.ServiceLabel: service.Container, dockerapi.ServiceSpecLabel: service.Spec},
	})
	if err != nil {
		return "", err //nolint:wrapcheck // the caller names the service
	}

	return id, client.StartContainer(ctx, id) //nolint:wrapcheck // the caller names the service
}

// waitReady runs the ready command until it exits 0, failing with the container's last lines when it stops or time runs out.
func waitReady(ctx context.Context, client *dockerapi.Client, id string, service Service) error {
	if len(service.Ready) == 0 {
		return nil
	}

	deadline := time.Now().Add(service.Timeout)

	for {
		attempt, cancel := context.WithDeadline(ctx, deadline)
		code, err := client.Exec(attempt, id, dockerapi.ExecOptions{Cmd: service.Ready, Stdout: io.Discard, Stderr: io.Discard})

		cancel()

		if err == nil && code == 0 {
			return nil
		}

		running, _, stateErr := client.ContainerState(ctx, id)
		if stateErr == nil && !running {
			return fmt.Errorf("service %s stopped before it was ready: %s", service.Container, client.ContainerLogTail(ctx, id, 20))
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("service %s %w: %v did not exit 0 within %s", service.Container, errNeverReady, service.Ready, service.Timeout)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("service %s: %w", service.Container, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// ServiceRunning reports whether the service's container is running with this configuration; a container steps did not create is an error, since a put would refuse it.
func ServiceRunning(ctx context.Context, container, spec string) (bool, error) {
	client, err := dockerapi.New("")
	if err != nil {
		return false, fmt.Errorf("service %s: %w", container, err)
	}

	defer func() { _ = client.Close() }()

	state, found, err := client.InspectService(ctx, container)
	if err != nil {
		return false, fmt.Errorf("service %s: %w", container, err)
	}

	if found && state.Spec == "" {
		return false, notOurs(container)
	}

	return found && state.Running && state.Spec == spec, nil
}

func notOurs(container string) error {
	return fmt.Errorf("container %s already exists and %w: remove or rename it, or give the resource another container", container, errNotOurs)
}
