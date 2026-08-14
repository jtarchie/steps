package config

// Resources and resource types: their schema, the get-step check that a
// referenced resource exists, and lookup by name.

import (
	"fmt"
	"log/slog"
	"strings"
)

// ResourceType defines a resource kind as a set of shell command templates.
type ResourceType struct {
	Name string `yaml:"name"`
	// Image, when set, runs check/in/out in a fresh `docker run --rm`
	// container from this image instead of on the host. Empty (the default)
	// keeps host execution, byte-identical to before this field existed.
	Image string `yaml:"image,omitempty"`
	// Env names host environment variables check/in/out are allowed to see,
	// on top of the always-allowed baseline (see shell.HostEnv). Names only —
	// see validateEnvValues. This is how a resource type reaches a registry
	// credential or deploy token without it being written into the pipeline.
	Env []string `yaml:"env,omitempty"`
	// User is the container user check/in/out execute as (docker's --user).
	// Empty takes the platform default — see shell's defaultContainerUser.
	User string `yaml:"user,omitempty"`
	// Network is the container network check/in/out join (docker's
	// --network); "none" cuts off egress entirely. Requires Image — and note
	// that most resource types exist to reach the network, so this is rarely
	// what you want on one.
	Network string `yaml:"network,omitempty"`
	// Privileged runs this command's container with `docker run --privileged`.
	// Mirrors Concourse's privileged: (concourse-ci.org/docs/steps/task/).
	// Container-only, like Network — a host command has nothing to elevate,
	// so it is a load error without image:.
	Privileged bool `yaml:"privileged,omitempty"`
	// Limits caps the container's CPU and memory. Mirrors Concourse's
	// container_limits:; container-only for the same reason Privileged is.
	Limits *ContainerLimits   `yaml:"container_limits,omitempty"`
	Config ResourceTypeConfig `yaml:"config"`
}

// ResourceTypeConfig holds the check/in/out shell command templates.
// Templates may reference {{ source.x }} and (for in/out) {{ version.y }}.
//
// MCP, when set, is mutually exclusive with Check/In/Out: this resource
// type's check/in/out are calls to a configured mcp_servers: entry instead
// of shell commands (see MCPResourceConfig, validateResourceTypeConfig).
type ResourceTypeConfig struct {
	Check string             `yaml:"check,omitempty"`
	In    string             `yaml:"in,omitempty"`
	Out   string             `yaml:"out,omitempty"`
	MCP   *MCPResourceConfig `yaml:"mcp,omitempty"`
}

// Resource is a named instance of a resource type, configured with a source.
type Resource struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Source map[string]any `yaml:"source"`
	// WebhookTokenEnv names an OS environment variable holding the shared
	// secret a webhook must present to trigger an immediate check of this
	// resource (see `steps watch --listen`).
	//
	// A REFERENCE, never the token itself — the same rule api_key_env follows,
	// and for a sharper reason here: a resource's fields are hashed into the
	// merkle content map, so a literal token would be written to state.db in
	// cleartext. That is precisely the trust-boundary problem the env-var
	// indirection exists to prevent.
	WebhookTokenEnv string `yaml:"webhook_token_env,omitempty"`
}

// validateResourcePut rejects a put step against a resource type that declares
// no way to put: an mcp-backed type with no out: tool, or a shell-backed type
// with no out: command.
//
// The shell half is what makes a built-in like `git` honest about being
// read-only — `put: repo` against it is a load error naming the reason, rather
// than a run that reaches the put and fails obscurely, or (worse) a type
// carrying a placeholder `out: "true"` that silently succeeds having pushed
// nothing. That placeholder is exactly the ritual this repo's own examples
// used to copy around.
func validateResourcePut(label, put string, resourceType *ResourceType) error {
	if resourceType.Config.MCP != nil {
		if resourceType.Config.MCP.Out == nil {
			return fmt.Errorf("%s: put %q targets mcp-backed resource type %q, which sets no mcp.out.tool; add one, or respond via an agent step granted the server's tools instead", label, put, resourceType.Name)
		}

		return nil
	}

	if strings.TrimSpace(resourceType.Config.Out) == "" {
		return fmt.Errorf("%s: put %q targets resource type %q, which declares no out: command; add one to describe what publishing means for this type", label, put, resourceType.Name)
	}

	return nil
}

// validateGetResource enforces that a step's resource: is only set on get
// steps and names an existing resource. The fetched resource is Resource when
// set, else Get (see Step.Resource); two get steps may alias the same resource
// under different names.
func (c *Config) validateGetResource() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Resource == "" {
				return nil
			}

			if step.Get == "" {
				return fmt.Errorf("%s: resource: is only valid on get steps", label)
			}

			_, err := c.FindResource(step.Resource)
			if err != nil {
				return fmt.Errorf("%s (get %q): %w", label, step.Get, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// FindResource returns the resource with the given name, or an error if not found.
func (c *Config) FindResource(name string) (*Resource, error) {
	slog.Debug("resource.find", "name", name)

	for i := range c.Resources {
		if c.Resources[i].Name == name {
			slog.Debug("resource.find", "name", name, "type", c.Resources[i].Type, "found", true)

			return &c.Resources[i], nil
		}
	}

	return nil, notFound("resource", name, names(c.Resources, func(r Resource) string { return r.Name }))
}

// ResourceNames lists every resource this job's plan touches — the resource
// a get fetches (GetResourceName, so an aliased get names the real one) and
// the resource a put publishes to — across plan steps and hooks alike, in
// plan order, deduplicated.
//
// The mirror of AgentNames, and it exists for the same reason: preflight
// checks only what THIS job needs. A pipeline with ten resources whose job
// touches two connects to two.
func (j Job) ResourceNames() []string {
	var names []string

	seen := map[string]bool{}

	add := func(name string) {
		if name == "" || seen[name] {
			return
		}

		seen[name] = true
		names = append(names, name)
	}

	_ = j.visitSteps(func(_ string, step *Step) error {
		add(step.GetResourceName())
		add(step.Put)

		return nil
	})

	return names
}

// PutSteps returns every step in this job's plan and hooks that publishes to
// the named resource.
//
// Preflight needs it to answer a question that is a property of the STEP, not
// of the resource: an mcp out: with no args: sends the put's own params: as
// the tool's arguments, so whether the tool's required arguments will be
// there depends on which put is asking. Mirrors StepsForAgent.
//
// Job-scoped is the useful scope for a `steps run`: whether THIS job can work
// cannot depend on how some other job spells its own put to a resource this
// one only gets. Config.PutSteps is the whole-pipeline form, for `steps
// watch`, which is going to run every job.
func (j Job) PutSteps(resource string) []Step {
	var steps []Step

	_ = j.visitSteps(func(_ string, step *Step) error {
		if step.Put == resource {
			steps = append(steps, *step)
		}

		return nil
	})

	return steps
}

// PutSteps returns every step in the pipeline that publishes to the named
// resource, across every job's plan and hooks — the whole-pipeline form of
// Job.PutSteps, whose doc comment explains when each scope is the right one.
func (c *Config) PutSteps(resource string) []Step {
	steps := make([]Step, 0, len(c.Jobs))

	for i := range c.Jobs {
		steps = append(steps, c.Jobs[i].PutSteps(resource)...)
	}

	return steps
}

// FindResourceType returns the resource type with the given name, or an error if not found.
func (c *Config) FindResourceType(name string) (*ResourceType, error) {
	slog.Debug("resource_type.find", "name", name)

	for i := range c.ResourceTypes {
		if c.ResourceTypes[i].Name == name {
			slog.Debug("resource_type.find", "name", name, "found", true)

			return &c.ResourceTypes[i], nil
		}
	}

	return nil, notFound("resource_type", name, names(c.ResourceTypes, func(rt ResourceType) string { return rt.Name }))
}
