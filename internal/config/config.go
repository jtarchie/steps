// Package config parses and resolves a Concourse-style pipeline YAML file
// (resource_types/resources/jobs) and the config-merge logic (task and
// agent-invocation resolution) that both plan-time hashing and run-time
// execution share.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Config is the top-level shape of a Concourse-style pipeline YAML file.
type Config struct {
	ResourceTypes []ResourceType `yaml:"resource_types"`
	Resources     []Resource     `yaml:"resources"`
	Agents        []Agent        `yaml:"agents"`
	MCPServers    []MCPServer    `yaml:"mcp_servers,omitempty"`
	Tasks         []Task         `yaml:"tasks"`
	Jobs          []Job          `yaml:"jobs"`
	// Defaults supplies pipeline-wide fallbacks — currently just the model
	// every agent uses when it names none. See Defaults.
	Defaults *Defaults `yaml:"defaults,omitempty"`
	// Workspace opts the pipeline into Concourse-style per-step isolation.
	// Absent (the default) keeps every step in a triggered build sharing one
	// mutable directory, exactly as before this field existed. See
	// WorkspaceConfig.
	Workspace *WorkspaceConfig `yaml:"workspace,omitempty"`
	// Assert, at the top level, names the ordered set of job names that
	// `steps test` must have run (see Assert). It's a self-verification
	// meta-check, never hashed.
	Assert *Assert `yaml:"assert,omitempty"`
}

// LoadConfig reads and parses a pipeline YAML file at path.
func LoadConfig(path string) (*Config, error) {
	slog.Debug("config.load", "path", path)

	data, err := os.ReadFile(path) //nolint:gosec // path is the pipeline file the user asked to run, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file %q: %w", path, err)
	}

	var cfg Config

	err = strictUnmarshal(data, &cfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse pipeline YAML %q: %w", path, err)
	}

	cfg.stampLines(data)

	slog.Info("config.loaded",
		"path", path,
		"resource_types", len(cfg.ResourceTypes),
		"resources", len(cfg.Resources),
		"jobs", len(cfg.Jobs),
	)

	err = cfg.resolveFileIncludes(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	cfg.registerBuiltinAgents()
	cfg.registerBuiltinResourceTypes()

	// After built-in registration, so a bare @builtin/<name> reference picks
	// up the default model without needing an agents: entry at all.
	cfg.applyDefaults()

	err = cfg.resolveSubAgentDescriptions()
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	err = cfg.validate()
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	return &cfg, nil
}

// validate checks schema-level invariants that the YAML decoder can't express
// on its own — in particular everything around workspace:/inputs:/outputs:,
// so a misconfigured pipeline fails at load time rather than mid-build.
//
// Every check runs and their errors are joined, rather than returning at the
// first one: a pipeline with four mistakes should take one run to find them
// all, not four. Each check still stops at its own first error, which keeps
// the walkers simple and the output short.
func (c *Config) validate() error {
	checks := []func() error{
		c.validateStepKinds,
		c.validateStepReferences,
		c.validateTaskInputsAll,
		c.validateStepFieldPlacement,
		c.validateTrySteps,
		c.validateWorkspace,
		c.validateArtifactDecls,
		c.validateGetResource,
		c.validateArtifactMappings,
		c.validateImageRules,
		c.validateTimeouts,
		c.validateAgentCompaction,
		c.validateAgentModels,
		c.validateAgentProviders,
		c.validateHooks,
		c.validateAgentGraph,
		c.validateToolCallGuards,
		c.validateStepGuards,
		c.validateStepContextPaths,
		c.validateHandoffNoteSteps,
		c.validateStepTransitions,
		c.validateAsserts,
		c.validateBudgets,
		c.validatePreflight,
		c.validateInParallel,
		c.validateCredentialHandling,
	}

	errs := make([]error, 0, len(checks))

	for _, check := range checks {
		err := check()
		if err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// validateCredentialHandling groups validateAgentEndpoints and every
// mcp_servers:-related check — split out of validate() itself to keep that
// function's branch count down (cyclop); all of it is trust-boundary
// validation around how a config references an external system's endpoint
// and credentials.
func (c *Config) validateCredentialHandling() error {
	err := c.validateAgentEndpoints()
	if err != nil {
		return err
	}

	err = c.validateMCPServers()
	if err != nil {
		return err
	}

	err = c.validateMCPToolGrants()
	if err != nil {
		return err
	}

	return c.validateResourceTypeConfig()
}
