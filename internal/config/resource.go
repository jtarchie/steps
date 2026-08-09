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
	Env    []string           `yaml:"env,omitempty"`
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
