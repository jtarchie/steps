package config

// type: docker-service: a long-lived container on this machine's daemon, on a named network, kept up by a put, for the steps that say network: <that network>. A test database a spec step reaches by name is the case it exists for. Go rather than YAML because it is the engine API, not the docker CLI.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// DockerServiceType is the built-in type that keeps a container up.
const DockerServiceType = "docker-service"

// defaultServiceReadyTimeout bounds how long a put waits for ready to succeed.
const defaultServiceReadyTimeout = 60 * time.Second

// DockerServiceSource is a docker-service resource's source:.
type DockerServiceSource struct {
	// Container is the container's name, which is also how steps on the network reach it.
	Container string `yaml:"container"`
	// Image should be pinned by digest: the put replaces the container only when its configuration changes, and a moving tag is not a change it can see.
	Image string `yaml:"image"`
	// Network is the bridge network the container joins, made if it is not there.
	Network string `yaml:"network"`
	// Env is the container's environment. Never a credential: it is hashed into the version, as any source: is.
	Env map[string]string `yaml:"env,omitempty"`
	// Volumes maps a named docker volume to the path it is mounted at, which is what keeps a database's data across a replaced container.
	Volumes map[string]string `yaml:"volumes,omitempty"`
	// Ready is a command run in the container until it exits 0; the put returns only then.
	Ready []string `yaml:"ready,omitempty"`
	// ReadyTimeout bounds the wait, a Go duration; 60s when unset.
	ReadyTimeout string `yaml:"ready_timeout,omitempty"`
}

// ParseDockerServiceSource decodes and checks a docker-service source:.
func ParseDockerServiceSource(raw map[string]any) (DockerServiceSource, error) {
	var source DockerServiceSource

	err := decodeSource(raw, &source)
	if err != nil {
		return source, err
	}

	for field, value := range map[string]string{"container": source.Container, "image": source.Image, "network": source.Network} {
		if strings.TrimSpace(value) == "" {
			return source, fmt.Errorf("a docker-service resource needs source.%s", field)
		}

		if strings.HasPrefix(value, "-") || strings.ContainsAny(value, " \t\n") {
			return source, fmt.Errorf("source.%s %q must not start with '-' or contain whitespace", field, value)
		}
	}

	for volume, path := range source.Volumes {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(volume, "/") {
			return source, fmt.Errorf("source.volumes: %q: %q must map a named volume to an absolute path in the container", volume, path)
		}
	}

	_, err = source.Timeout()

	return source, err
}

// Timeout is how long a put waits for ready.
func (s DockerServiceSource) Timeout() (time.Duration, error) {
	if s.ReadyTimeout == "" {
		return defaultServiceReadyTimeout, nil
	}

	timeout, err := time.ParseDuration(s.ReadyTimeout)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("source.ready_timeout %q is not a positive duration such as 60s", s.ReadyTimeout)
	}

	return timeout, nil
}

// Spec is the hash of everything that decides what container runs. A put compares it with the running container's and replaces the container only when it differs; it is also the version.
func (s DockerServiceSource) Spec() string {
	canonical, _ := json.Marshal(struct { //nolint:errchkjson // strings and string maps: Marshal cannot fail on them
		Container string            `json:"container"`
		Image     string            `json:"image"`
		Network   string            `json:"network"`
		Env       map[string]string `json:"env"`
		Volumes   map[string]string `json:"volumes"`
	}{s.Container, s.Image, s.Network, s.Env, s.Volumes})

	sum := sha256.Sum256(canonical)

	return hex.EncodeToString(sum[:8])
}

func (c *Config) validateDockerServiceResources() error {
	for _, resource := range c.Resources {
		if c.nativeType(resource.Type) != DockerServiceType {
			continue
		}

		_, err := ParseDockerServiceSource(resource.Source)
		if err != nil {
			return fmt.Errorf("resource %q (docker-service): %w", resource.Name, err)
		}
	}

	err := c.validateDockerServicePuts()
	if err != nil {
		return err
	}

	return c.validateServiceNetworkPlacement()
}

// serviceNetworks is each docker-service network, with the resource that keeps it.
func (c *Config) serviceNetworks() map[string]string {
	networks := map[string]string{}

	for _, resource := range c.Resources {
		if c.nativeType(resource.Type) != DockerServiceType {
			continue
		}

		source, err := ParseDockerServiceSource(resource.Source)
		if err == nil {
			networks[source.Network] = resource.Name
		}
	}

	return networks
}

// validateServiceNetworkPlacement refuses a step placed on a worker that joins a service's network: the service runs on this machine's daemon, and a network is one daemon's.
func (c *Config) validateServiceNetworkPlacement() error {
	networks := c.serviceNetworks()
	if len(networks) == 0 {
		return nil
	}

	return c.visitPlacements(func(label string, step *Step, placed placement) error {
		if len(placed.tags) == 0 {
			return nil
		}

		network := c.stepNetwork(*step)

		resource, ok := networks[network]
		if !ok {
			return nil
		}

		return placed.hint(fmt.Errorf("%s: network %q is docker-service %q's, which runs on this machine's daemon, and the step is placed on worker %s, whose daemon has no such network",
			placed.describe(label), network, resource, strings.Join(placed.tags, ", ")))
	})
}

// stepNetwork is the network a task or agent step's commands join, "" for any other step.
func (c *Config) stepNetwork(step Step) string {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		task, err := c.ResolveTask(step)
		if err == nil {
			return task.Network
		}
	case StepKindAgent:
		invocation, err := c.ResolveAgentInvocation(step)
		if err == nil {
			return invocation.Network
		}
	case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindAsk, StepKindDo:
	}

	return step.Network
}

// errServiceParams is a docker-service put given params:, which it reads none of.
var errServiceParams = errors.New("a docker-service put takes no params; what runs is its source")

func (c *Config) validateDockerServicePuts() error {
	for i := range c.Jobs {
		err := c.Jobs[i].visitSteps(func(label string, step *Step) error {
			if step.Put == "" || len(step.Params) == 0 || c.nativeResourceType(step.PutResourceName()) != DockerServiceType {
				return nil
			}

			return fmt.Errorf("%s: put %q: %w", label, step.Put, errServiceParams)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// SortedEnv is source.env as NAME=value pairs in a stable order.
func (s DockerServiceSource) SortedEnv() []string {
	env := make([]string, 0, len(s.Env))
	for _, name := range slices.Sorted(maps.Keys(s.Env)) {
		env = append(env, name+"="+s.Env[name])
	}

	return env
}
