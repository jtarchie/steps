package config

// Native resource types: the ones steps ships (git, cron, webhook, the slack-* and github-* types). Their source: and params: are closed, a deliberate divergence from Concourse, whose resources are opaque images: steps owns every native type's code, so it knows each key a type reads and refuses the rest at load. Each type's shape is a struct decoded through decodeSource/decodeParams in the file that owns the type; this file holds what every native type shares.

import (
	"fmt"
	"reflect"
)

// nativeType is the native type typeName resolves to, "" for a type the pipeline wrote. A pipeline that declares its own git or slack-* type replaces the built-in (registerBuiltinResourceTypes), so a YAML-backed type is native only while it is still the embedded one.
func (c *Config) nativeType(typeName string) string {
	resourceType, err := c.FindResourceType(typeName)
	if err != nil {
		return ""
	}

	switch resourceType.Config.Backend() {
	case BackendWebhook:
		return WebhookType
	case BackendCron:
		return CronType
	case BackendGitHub:
		return resourceType.Config.GitHub
	case BackendShell, BackendExpr, BackendMCP:
	}

	builtin, err := ReadBuiltinResourceType(resourceType.Name)
	if err != nil || !reflect.DeepEqual(builtin.Config, resourceType.Config) {
		return ""
	}

	return resourceType.Name
}

// nativeResourceType is the native type the named resource is, "" when it is not one or does not exist (the rule that owns unknown resources reports that).
func (c *Config) nativeResourceType(name string) string {
	resource, err := c.FindResource(name)
	if err != nil {
		return ""
	}

	return c.nativeType(resource.Type)
}

// validateNativeGets refuses params: on a get from any native type: none reads them, so a Concourse habit like depth: 1 would load and do nothing.
func (c *Config) validateNativeGets() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Get == "" || len(step.Params) == 0 {
				return nil
			}

			kind := c.nativeResourceType(step.GetResourceName())
			if kind == "" {
				return nil
			}

			return fmt.Errorf("%s: get %q: a %s get takes no params; what it fetches is set on the resource's source instead", label, step.Get, kind)
		})
		if err != nil {
			return err
		}
	}

	return nil
}
