package resource

// The built-in docker-service type: out keeps a container up, check says whether it is, in names it.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

// serviceSpecField is the one key of a docker-service version: the hash of the configuration the container runs.
const serviceSpecField = "spec"

type serviceBackend struct{}

// check is the running configuration, or no versions when the container is down or runs another one: a get is the only thing a version would be read for, and none should find a service that is not there.
func (serviceBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	source, err := config.ParseDockerServiceSource(c.Resource.Source)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", c.Resource.Name, err)
	}

	running, err := shell.ServiceRunning(ctx, source.Container, source.Spec())
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", c.Resource.Name, err)
	}

	if !running {
		return nil, nil
	}

	return []map[string]any{{serviceSpecField: source.Spec()}}, nil
}

// in names the container and its network, which is all a step needs to reach it.
func (serviceBackend) in(_ context.Context, c Call) error {
	source, err := config.ParseDockerServiceSource(c.Resource.Source)
	if err != nil {
		return fmt.Errorf("in %q: %w", c.Resource.Name, err)
	}

	for file, content := range map[string]string{"container": source.Container, "network": source.Network} {
		err := os.WriteFile(filepath.Join(c.Dir, file), []byte(content), 0o644) //nolint:gosec // names a step reads, not a secret
		if err != nil {
			return fmt.Errorf("in %q: %w", c.Resource.Name, err)
		}
	}

	return nil
}

// out ensures the service and returns once it is ready.
func (serviceBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	source, err := config.ParseDockerServiceSource(c.Resource.Source)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	timeout, err := source.Timeout()
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	err = shell.EnsureService(ctx, shell.Service{
		Container: source.Container, Image: source.Image, Network: source.Network,
		Env: source.SortedEnv(), Volumes: source.Volumes, Ready: source.Ready, Timeout: timeout, Spec: source.Spec(),
	}, events.Stdout(ctx))
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", c.Resource.Name, err)
	}

	slog.InfoContext(ctx, "resource.put", "resource_type", c.Type.Name, "container", source.Container, "spec", source.Spec())

	return map[string]any{serviceSpecField: source.Spec()}, nil
}
