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
	// the same reason store.Store holds its pipeline_id: the scope has to be
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

// Revision is one configuration, as parsed: the substituted source and its
// hash.
//
// The source is carried rather than re-read on demand because only the loader
// ever holds it — by the time anything asks, the file on disk may be a later
// revision, which is precisely the situation this exists to answer.
type Revision struct {
	SHA    string
	Source string
	// Includes are the files whose contents this configuration folded in —
	// every run_file:, system_file:, message file, task file and agent file it
	// resolved — named as the pipeline names them, relative to its own
	// directory. They are part of the hash, and a caller re-checking whether
	// the configuration has changed has to re-read them; see withIncludes.
	//
	// Relative rather than resolved, because the path STRING is hashed: a
	// resolved one carries how the pipeline was invoked, so the same file
	// loaded as ci/app.yml and as /repo/ci/app.yml produced two different
	// configuration hashes and a run page reporting an edit that never
	// happened.
	Includes []string
}

// withIncludes folds the resolved include files into the revision, so an edit
// to one is an edit to the pipeline. Paths are relative to baseDir, which is
// the pipeline file's own directory.
//
// The digest describes path→content PAIRS rather than a concatenation of
// bodies. Sorting by path already makes the concatenation order stable, so
// nothing reachable today distinguishes the two — this is what keeps that
// true if the ordering ever stops being by path.
func (r Revision) withIncludes(baseDir string, paths []string) (Revision, error) {
	if len(paths) == 0 {
		return r, nil
	}

	// Sorted and de-duplicated, so the hash describes the SET of includes
	// rather than the order the resolver happened to walk the config in — a
	// step moved from one job to another changes that order and nothing else.
	unique := slices.Clone(paths)
	slices.Sort(unique)
	unique = slices.Compact(unique)

	sum := sha256.New()
	sum.Write([]byte(r.Source))

	for _, path := range unique {
		full := filepath.Join(baseDir, path)

		body, err := os.ReadFile(full) //nolint:gosec // the loader just read this same file to build the config
		if err != nil {
			return Revision{}, fmt.Errorf("could not read included file %q: %w", full, err)
		}

		sum.Write([]byte("\x00" + path + "\x00"))
		sum.Write(body)
	}

	r.SHA = hex.EncodeToString(sum.Sum(nil))
	r.Includes = unique

	return r, nil
}

// Recorded reports whether this Config was loaded from a file (rather than
// built in a test), which is what makes its revision worth writing down.
func (r Revision) Recorded() bool { return r.SHA != "" }

// FileRevision answers WHICH configuration a file currently is — the
// substituted bytes and their hash — without parsing them.
//
// Split out of Load so a caller that only needs "has this changed?" can pay
// the reads and a hash for it. `steps web` asks once a second, and asking
// through Load meant a full parse, every validator and an exec.LookPath per
// stdio MCP server on every tick — a measured few milliseconds of CPU and a
// log line per pipeline per second, all of it to conclude that nothing had
// changed.
//
// Load is written on top of this rather than beside it: one definition of the
// hash is what stops the cheap answer and the parsed one from disagreeing.
func FileRevision(path string, vars map[string]string, includes []string) (Revision, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the pipeline file the user asked to run, not untrusted input
	if err != nil {
		return Revision{}, fmt.Errorf("could not read pipeline file %q: %w", path, err)
	}

	data = InterpolateVars(data, vars)

	sum := sha256.Sum256(data)
	revision := Revision{SHA: hex.EncodeToString(sum[:]), Source: string(data)}

	// The includes a caller already knows about, which is the set the
	// configuration it is holding resolved. An edit that changes WHICH files
	// are included changes the YAML too, so it is caught by the hash above
	// before this list is out of date.
	return revision.withIncludes(filepath.Dir(path), includes)
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

// Load reads and parses the pipeline YAML at path under the identity name,
// with ((name)) substitution applied to the source before it is parsed.
//
// name is a parameter rather than something derived here because the identity
// is the caller's to decide: `--name prod=infra/deploy.yml` is an operator
// saying which pipeline this is, and it must reach the store, the /p/<slug>
// route and the Config as one string. Positional, so a call site cannot
// quietly skip it — the same reason store.OpenStore takes one.
//
// Substituting before the parse is what lets a var appear anywhere a value
// does — inside a URI, mid-command, as a whole mapping value — without this
// package enumerating every field that might contain one.
//
// ⚠️ A substituted value is ORDINARY CONFIG: it is parsed, hashed, and stored
// in state.db like anything else written in the file. Vars separate a
// pipeline's shape from its parameters; they are not a secret store. Keep
// credentials in the env-var references (api_key_env:) that exist for them.
func Load(path string, name string, vars map[string]string) (*Config, error) {
	slog.Debug("config.load", "path", path)

	// No includes yet — they are not known until the parse below resolves
	// them, and are folded in there (see withIncludes).
	revision, err := FileRevision(path, vars, nil)
	if err != nil {
		return nil, err
	}

	data := []byte(revision.Source)

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

	includes, err := cfg.resolveFileIncludes(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	// The revision covers the includes, not just the YAML. A run_file: or a
	// system_file: decides what a step executes, so a hash over the pipeline
	// file alone answered "did the pipeline change?" with a confident no for
	// the edit that changed everything — the CONFIG column stayed put across
	// runs that ran different code.
	revision, err = revision.withIncludes(filepath.Dir(path), includes)
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

	// Before validate(), so the across: it writes is checked like any
	// hand-written matrix — see desugarParallelism. Joined rather than
	// returned first: a rejected step is simply left unrewritten, and hiding
	// validate()'s independent findings behind a desugar mistake would cost
	// the extra load the joined-errors contract below exists to save.
	err = errors.Join(cfg.desugarParallelism(), cfg.validate())
	if err != nil {
		return nil, fmt.Errorf("pipeline YAML %q: %w", path, err)
	}

	cfg.inheritResourceTags()

	cfg.Name = name
	cfg.Revision = revision

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
