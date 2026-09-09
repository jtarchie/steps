// Package config parses and resolves a Concourse-style pipeline YAML file
// (resource_types/resources/jobs) and the config-merge logic (task and
// agent-invocation resolution) that both plan-time hashing and run-time
// execution share.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	// Name is WHICH pipeline this is, stamped by the loader. Not YAML, never
	// hashed: it is identity, not content.
	//
	// It exists because one process can serve several pipelines (`steps web
	// app.yml infra.yml`), and process-wide state keyed by a name a pipeline
	// chose — an agent, a job — collides across them. The Config holds it for
	// the same reason a store.Store handle holds its pipeline: the scope has to be
	// impossible to forget.
	//
	// It is the SAME STRING the store's pipelines.name and the web UI's
	// /p/<slug> route use, and that is the whole point of it being supplied
	// rather than derived here. A path was the obvious discriminator and was
	// the wrong one: it is a second identity that disagrees with the one the
	// repo already resolves and publishes, so a pin log line said
	// /abs/infra/deploy.yml where every run record said deploy — and --name,
	// which exists precisely to say which pipeline this is, moved the store
	// and the route and not the pin.
	//
	// A Config built in a test rather than loaded has an empty Name, which
	// shares one scope with every other such Config, exactly as everything
	// did before this field existed.
	Name string `yaml:"-"`
	// Revision is WHICH configuration this is: the bytes it was parsed from,
	// and their hash. Stamped by the loader, never YAML, never hashed into a
	// node — it identifies the content, so folding it into a step's content
	// would make every step of an edited file re-run, which is the merkle
	// cache's job to decide and not this field's.
	//
	// Computed AFTER ((var)) substitution, because substitution happens
	// before the parse: one file under two --vars-files is two
	// configurations, and a revision taken from the file on disk would call
	// them one.
	//
	// A Config built in a test rather than loaded has an empty Revision, and
	// a run started from one records no revision at all — see
	// store.Store.RecordRevision.
	Revision Revision `yaml:"-"`
}

// Revision is one configuration, as parsed: the substituted source, the include contents it folded in, and the hash over both.
type Revision struct {
	SHA    string
	Source string
	// Includes is path→content, the path exactly as the pipeline names it: the string is hashed, so a resolved path made ci/app.yml and /repo/ci/app.yml two configurations of the same bytes.
	Includes map[string]string
}

// withIncludes hashes path→content PAIRS in sorted path order, so the hash describes the SET of includes rather than the order the resolver walked the config in.
func (r Revision) withIncludes(includes map[string]string) Revision {
	sum := sha256.New()
	sum.Write([]byte(r.Source))

	if len(includes) > 0 {
		for _, path := range slices.Sorted(maps.Keys(includes)) {
			sum.Write([]byte("\x00" + path + "\x00"))
			sum.Write([]byte(includes[path]))
		}

		r.Includes = includes
	}

	r.SHA = hex.EncodeToString(sum.Sum(nil))

	return r
}

// Recorded reports whether this Config was loaded from a source (rather than built in a test), which is what makes its revision worth writing down.
func (r Revision) Recorded() bool { return r.SHA != "" }

// ReadSource reads the pipeline file at path with ((name)) substitution applied: the bytes a revision is taken over, and what `steps pipeline set` uploads.
func ReadSource(path string, vars map[string]string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the pipeline file the user asked to run, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file %q: %w", path, err)
	}

	return InterpolateVars(data, vars), nil
}

// Slugify turns a pipeline path into its identity: the base name without its
// extension.
//
// It lives here, and web.Slugify calls it, because a second copy is how the
// identity split the first time. internal/web cannot be imported from this
// package, and the string is needed on both sides.
func Slugify(path string) string {
	base := filepath.Base(path)

	return base[:len(base)-len(filepath.Ext(base))]
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

// LoadConfigWithVars is LoadConfig with vars, under the same default
// identity.
func LoadConfigWithVars(path string, vars map[string]string) (*Config, error) {
	return Load(path, Slugify(path), vars)
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

	err := strictUnmarshal(source, &cfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse pipeline YAML %q: %w", label, err)
	}

	cfg.stampLines(source)

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

	cfg.inheritResourceTags()

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
		c.validateStepReferences,
		c.validateTaskInputsAll,
		c.validateStepFieldPlacement,
		c.validateTrySteps,
		c.validateWorkspace,
		c.validateArtifactDecls,
		c.validateGetResource,
		c.validateArtifactMappings,
		c.validateImageRules,
		c.validateEnvRules,
		c.validateUserRules,
		c.validateNetworkRules,
		c.validateTagRules,
		c.validateLimitsRules,
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
		c.validateVolatileSteps,
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
		c.validateWebhookTokens,
		c.validateApprovals,
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
