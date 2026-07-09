// Package main implements steps, a small CLI that interprets a
// Concourse-style pipeline YAML file (resource_types/resources/jobs):
// check discovers resource versions, get fetches one via a rendered
// shell command, and task runs a plan step's command.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level shape of a Concourse-style pipeline YAML file.
type Config struct {
	ResourceTypes []ResourceType `yaml:"resource_types"`
	Resources     []Resource     `yaml:"resources"`
	Jobs          []Job          `yaml:"jobs"`
}

// ResourceType defines a resource kind as a set of shell command templates.
type ResourceType struct {
	Name   string             `yaml:"name"`
	Config ResourceTypeConfig `yaml:"config"`
}

// ResourceTypeConfig holds the check/in/out shell command templates.
// Templates may reference {{ source.x }} and (for in/out) {{ version.y }}.
type ResourceTypeConfig struct {
	Check string `yaml:"check"`
	In    string `yaml:"in"`
	Out   string `yaml:"out"`
}

// Resource is a named instance of a resource type, configured with a source.
type Resource struct {
	Name   string         `yaml:"name"`
	Type   string         `yaml:"type"`
	Source map[string]any `yaml:"source"`
}

// Job is a named sequence of steps to run.
type Job struct {
	Name string `yaml:"name"`
	Plan []Step `yaml:"plan"`
}

// Step is a flat union of the step kinds this interpreter supports: get and task.
type Step struct {
	Get     string `yaml:"get,omitempty"`
	Trigger bool   `yaml:"trigger,omitempty"`
	// Version selects which version(s) a get step fetches: unset/"latest"
	// (default) picks the single latest version; "every" runs the rest of
	// the plan once per version returned by check; a map pins to a specific
	// version. Mirrors Concourse's get.version field.
	Version any    `yaml:"version,omitempty"`
	Task    string `yaml:"task,omitempty"`
	Run     string `yaml:"run,omitempty"`
}

// LoadConfig reads and parses a pipeline YAML file at path.
func LoadConfig(path string) (*Config, error) {
	slog.Debug("config.load", "path", path)

	data, err := os.ReadFile(path) //nolint:gosec // path is the pipeline file the user asked to run, not untrusted input
	if err != nil {
		err = fmt.Errorf("could not read pipeline file %q: %w", path, err)
		slog.Error("config.load", "path", path, "error", err)

		return nil, err
	}

	var cfg Config

	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		err = fmt.Errorf("could not parse pipeline YAML %q: %w", path, err)
		slog.Error("config.parse", "path", path, "error", err)

		return nil, err
	}

	slog.Info("config.loaded",
		"path", path,
		"resource_types", len(cfg.ResourceTypes),
		"resources", len(cfg.Resources),
		"jobs", len(cfg.Jobs),
	)

	return &cfg, nil
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

	err := fmt.Errorf("no resource named %q", name)
	slog.Error("resource.find", "name", name, "error", err)

	return nil, err
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

	err := fmt.Errorf("no resource_type named %q", name)
	slog.Error("resource_type.find", "name", name, "error", err)

	return nil, err
}

// FindJob returns the job with the given name, or an error if not found.
func (c *Config) FindJob(name string) (*Job, error) {
	slog.Debug("job.find", "name", name)

	for i := range c.Jobs {
		if c.Jobs[i].Name == name {
			slog.Debug("job.find", "name", name, "steps", len(c.Jobs[i].Plan), "found", true)

			return &c.Jobs[i], nil
		}
	}

	names := make([]string, 0, len(c.Jobs))
	for _, j := range c.Jobs {
		names = append(names, j.Name)
	}

	err := fmt.Errorf("no job named %q (available: %v)", name, names)
	slog.Error("job.find", "name", name, "available", names, "error", err)

	return nil, err
}
