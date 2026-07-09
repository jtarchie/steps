package main

import (
	"fmt"
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
	Task    string `yaml:"task,omitempty"`
	Run     string `yaml:"run,omitempty"`
}

// LoadConfig reads and parses a pipeline YAML file at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file %q: %w", path, err)
	}

	var cfg Config

	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse pipeline YAML %q: %w", path, err)
	}

	return &cfg, nil
}

// FindResource returns the resource with the given name, or an error if not found.
func (c *Config) FindResource(name string) (*Resource, error) {
	for i := range c.Resources {
		if c.Resources[i].Name == name {
			return &c.Resources[i], nil
		}
	}

	return nil, fmt.Errorf("no resource named %q", name)
}

// FindResourceType returns the resource type with the given name, or an error if not found.
func (c *Config) FindResourceType(name string) (*ResourceType, error) {
	for i := range c.ResourceTypes {
		if c.ResourceTypes[i].Name == name {
			return &c.ResourceTypes[i], nil
		}
	}

	return nil, fmt.Errorf("no resource_type named %q", name)
}

// FindJob returns the job with the given name, or an error if not found.
func (c *Config) FindJob(name string) (*Job, error) {
	for i := range c.Jobs {
		if c.Jobs[i].Name == name {
			return &c.Jobs[i], nil
		}
	}

	names := make([]string, 0, len(c.Jobs))
	for _, j := range c.Jobs {
		names = append(names, j.Name)
	}

	return nil, fmt.Errorf("no job named %q (available: %v)", name, names)
}
