// Package config parses and resolves a Concourse-style pipeline YAML file
// (resource_types/resources/jobs) and the config-merge logic (task and
// agent-invocation resolution) that both plan-time hashing and run-time
// execution share.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
)

// Config is the top-level shape of a Concourse-style pipeline YAML file.
type Config struct {
	ResourceTypes []ResourceType `yaml:"resource_types"`
	Resources     []Resource     `yaml:"resources"`
	Agents        []Agent        `yaml:"agents"`
	MCPServers    []MCPServer    `yaml:"mcp_servers,omitempty"`
	// MCPExports are bundles of this pipeline's tools other people's agents may connect to through the daemon. See MCPExport.
	MCPExports []MCPExport `yaml:"mcp_exports,omitempty"`
	// Rules are named checks on a call to a shared or granted tool. See Rule.
	Rules []Rule `yaml:"rules,omitempty"`
	Tasks []Task `yaml:"tasks"`
	Jobs  []Job  `yaml:"jobs"`
	// Defaults supplies pipeline-wide fallbacks and bounds: the model an
	// agent uses when it names none, preflight tuning, the delegation budget,
	// and the version/run/memory caps. See Defaults.
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
	// Access narrows who may see and operate this pipeline on a daemon people
	// sign in to. See Access.
	Access *Access `yaml:"access,omitempty"`
	// Name is WHICH pipeline this is, stamped by the loader, never YAML, never hashed (identity, not content). It is the SAME STRING store pipelines.name and the web /p/<slug> route use, supplied rather than derived from a path because a path is a second identity that disagrees with the published one (--name moved the store and the route but not the pin). A Config built in a test has an empty Name and shares one scope with every other such Config.
	Name string `yaml:"-"`
	// Revision is WHICH configuration this is, stamped by the loader, never YAML, never hashed into a node: folding it into step content would re-run every step of an edited file, which is the merkle cache's call. Computed AFTER ((var)) substitution, so one file under two --vars-files is two configurations. A Config built in a test has an empty Revision and records none (store.Store.RecordRevision).
	Revision Revision `yaml:"-"`
}

// LoadConfig reads and parses a pipeline YAML file at path, under the
// identity its file name implies.
//
// The convenience form. A caller that resolves identities of its own — main,
// which applies the --name overrides — calls Load and says which one, so the
// Config cannot end up under a name nothing else uses.
func LoadConfig(path string) (*Config, error) {
	return Load(path, Slugify(path), nil)
}

// Load is the on-disk form of Parse: substitution happens BEFORE the parse so a var may appear anywhere a value does, and a substituted value is ORDINARY CONFIG, hashed and stored — vars are parameters, never secrets.
func Load(path string, name string, vars map[string]string) (*Config, error) {
	source, err := ReadSource(path, vars)
	if err != nil {
		return nil, err
	}

	return parseSource(source, name, path, DirFS(filepath.Dir(path)))
}

// Parse builds a Config from already-substituted source with its includes read through fsys, which is what a daemon does with a `steps pipeline set` upload it has no directory for.
func Parse(source []byte, name string, fsys Files) (*Config, error) {
	return parseSource(source, name, name, fsys)
}

// parseSource takes the identity separately from the label because a daemon's errors cite the name it was given and a local load cites the path that was typed.
func parseSource(source []byte, name string, label string, fsys Files) (*Config, error) {
	slog.Debug("config.load", "pipeline", label)

	var cfg Config

	expanded, remap := expandInParallelShorthand(source)

	err := strictUnmarshal(expanded, &cfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse pipeline YAML %q: %w", label, remap(err))
	}

	cfg.stampLines(source)

	err = cfg.validateUniqueNames(source)
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", label, err)
	}

	slog.Info("config.loaded",
		"pipeline", label,
		"resource_types", len(cfg.ResourceTypes),
		"resources", len(cfg.Resources),
		"jobs", len(cfg.Jobs),
	)

	includes, err := cfg.resolveFileIncludes(fsys)
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", label, err)
	}

	cfg.registerBuiltinAgents()
	cfg.registerBuiltinResourceTypes()

	// After built-in registration, so a bare @builtin/<name> reference picks up the default model without an agents: entry.
	cfg.applyDefaults()

	err = cfg.resolveSubAgentDescriptions()
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", label, err)
	}

	// Desugar before validate() so the across: it writes is checked like a hand-written matrix; joined so a desugar mistake does not hide validate()'s own findings.
	err = errors.Join(cfg.desugarParallelism(), cfg.validate())
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", label, err)
	}

	cfg.resolveTags()

	cfg.Name = name
	// The revision covers the includes, not just the YAML: a run_file: decides what a step executes, so a hash over the file alone said "unchanged" for the edit that changed everything.
	cfg.Revision = Revision{Source: string(source)}.withIncludes(includes)

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
		c.validateAccess,
		c.validateMCPExports,
		c.validateRules,
		c.validateStepReferences,
		c.validateTaskInputsAll,
		c.validateStepFieldPlacement,
		c.validateTrySteps,
		c.validateWorkspace,
		c.validateArtifactDecls,
		c.validateResourceTypes,
		c.validateStepResource,
		c.validateArtifactMappings,
		c.validateImageRules,
		c.validateEnvRules,
		c.validateSecrets,
		c.validateUserRules,
		c.validateNetworkRules,
		c.validateTagRules,
		c.validateLimitsRules,
		c.validateSandbox,
		c.validateTimeouts,
		c.validateAgentCompaction,
		c.validateAgentModels,
		c.validateAgentProviders,
		c.validateCLIAgents,
		c.validateHooks,
		c.validateAgentGraph,
		c.validateToolCallGuards,
		c.validateAskUserResponders,
		c.validateStepGuards,
		c.validateStepContextPaths,
		c.validateMaxContextBytes,
		c.validateAgentDials,
		c.validateAttempts,
		c.validateContextSteps,
		c.validateContextFrom,
		c.validateMemory,
		c.validateVolatileSteps,
		c.validateCaches,
		c.validateStepTransitions,
		c.validateAsserts,
		c.validateDelegateBudgets,
		c.validateBudgets,
		c.validatePreflight,
		c.validateInParallel,
		c.validateDo,
		c.validateRace,
		c.validateEnsemble,
		c.validateAcross,
		c.validatePassed,
		c.validateVersionEvery,
		c.validateSerial,
		c.validateVars,
		c.validateWebhookResources,
		c.validateCronResources,
		c.validateGitHubResources,
		c.validateSlackResources,
		c.validateNativeGets,
		c.validateGitResources,
		c.validateGitPuts,
		c.validateApprovals,
		c.validateAgentEndpoints,
		c.validateMCPServers,
		c.validateMCPToolGrants,
		c.validateResourceTypeConfig,
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
