package config

// A plan step: the flat get/task/put/agent union, its inputs: declaration,
// its prompt_file: reference, and the walk over a step and its hook tree.

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Step is a flat union of the step kinds this interpreter supports: get,
// task, put, and agent.
type Step struct {
	Get     string `yaml:"get,omitempty"`
	Trigger bool   `yaml:"trigger,omitempty"`
	// Version selects which version(s) a get step fetches: unset/"latest"
	// (default) picks the single latest version; "every" runs the rest of
	// the plan once per version returned by check; a map pins to a specific
	// version. Mirrors Concourse's get.version field, including that a
	// failed version's build does not stop "every" from attempting the
	// remaining versions — see docs/conformance.md and
	// internal/pipeline/conformance_test.go's
	// TestConformanceGetVersionEveryContinuesPastFailure.
	Version any `yaml:"version,omitempty"`
	// Task labels a task step. If Run is also set, the step is inline and
	// Run/Fix below are used as-is. If Run is empty, Task instead names a
	// tasks: entry (see Task) to resolve run/fix from; this step's own Fix,
	// if set, overrides the referenced task's Fix for this step only.
	Task string `yaml:"task,omitempty"`
	Run  string `yaml:"run,omitempty"`
	// RunFile loads Run's text from a file at a path relative to the pipeline
	// file's directory, instead of writing it inline (see LoadConfig's
	// resolveFileIncludes). Mutually exclusive with Run. Task steps only.
	RunFile string `yaml:"run_file,omitempty"`
	// Fix, on a task step, names an agent to invoke when run: exits nonzero:
	// the agent is seeded with the captured output and given the task itself
	// as a rerun tool, then the command is re-run to decide the step. A green
	// run never constructs the agent. See FixSpec.
	Fix *FixSpec `yaml:"fix,omitempty"`
	// Put names a resource to run its out command against.
	Put string `yaml:"put,omitempty"`
	// Params are passed through to the resource command this step runs, as
	// {{ .params.x }}: a put's reach out:, a get's reach in:. Mirrors
	// Concourse, where params on a get are how a resource is told HOW to
	// fetch (git's depth:/submodules:, s3's unpack:) — see
	// concourse-ci.org/docs/steps/get/ and docs/conformance.md.
	//
	// A get's params fold into its node hash (see merkle.GetNodeContent):
	// they change what lands in the artifact, so two gets of one version
	// differing in params are two different fetches and must not share a
	// cache entry.
	Params map[string]any `yaml:"params,omitempty"`
	// GetParams configures the implicit get: a put runs after its out:
	// succeeds, mirroring Concourse (concourse-ci.org/docs/steps/put/). They
	// reach that fetch's in: exactly as a get step's own params: do, and are
	// meaningless without it — setting them alongside no_get: true is a load
	// error rather than a line that quietly does nothing.
	GetParams map[string]any `yaml:"get_params,omitempty"`
	// NoGet skips the implicit get after a put. Concourse's own escape hatch,
	// for a put at the end of a plan whose produced version nothing goes on to
	// use: the fetch costs a round trip and a directory for an artifact
	// nobody reads.
	NoGet bool `yaml:"no_get,omitempty"`
	// Agent names an agents: entry this step invokes. Prompt is the task
	// given to the model (not templated — freeform text is likely to contain
	// literal {{ }} that isn't meant as a template). Dir is the step's
	// working directory relative to the job's workspace, since there's no
	// run: string to embed a cd in. Tools selects a subset of the agent's
	// granted tools by name and may add inline custom tools for this task
	// (empty means all of the agent's tools). Attempts overrides the agent's
	// default retry count (attempts: 3 = up to 3 total tries, including the
	// first); 0/unset inherits the agent's default (which itself defaults
	// to 1).
	Agent  string `yaml:"agent,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
	// PromptFile supplies Prompt from a file instead of inline text, in one of
	// two forms (see FileRef): a scalar path, relative to the pipeline file's
	// directory, resolved at load time exactly like RunFile/SystemFile; or a
	// {artifact, path} mapping naming a file inside an artifact this step
	// declares in its inputs:, read at run time once that artifact is
	// fetched. The run-time form exists only here (not for a task's run:, not
	// for an agent's own definition/persona) — see docs/agents.md. It costs
	// no merkle caching: an agent step's chain is already unconditionally
	// unskippable (see internal/merkle's planNonGetNode), so there is nothing
	// to lose by resolving this after plan time. Mutually exclusive with
	// Prompt in the scalar form.
	PromptFile *FileRef   `yaml:"prompt_file,omitempty"`
	Dir        string     `yaml:"dir,omitempty"`
	Tools      []ToolSpec `yaml:"tools,omitempty"`
	Attempts   int        `yaml:"attempts,omitempty"`
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// (default) means no timeout. Valid on all step kinds (a get step's
	// timeout bounds both its check and in commands); for task/put steps it
	// overrides the referenced task/agent's Timeout.
	Timeout string `yaml:"timeout,omitempty"`
	// Try wraps an inner step so a task-level failure of that step doesn't
	// stop the plan. The wrapper is transparent: the inner step runs exactly
	// as it would unwrapped (its when: guard, hooks and outcome are all real),
	// and everything that observes the outcome — the wrapper's own hooks, its
	// to: route, the recorded node — sees the truth. Only the plan walker is
	// lied to, and only for an outcome.Failed inner step: an infrastructure
	// error or an abort still stops the run (see internal/pipeline's
	// tolerateTryFailure). Composes with attempts:/timeout: on the inner step,
	// and with try: itself (a doubled try: is legal, if pointless).
	//
	// The wrapper is the plan-positioned step, so to:/max_visits: belong on it
	// and are rejected on the step it wraps; verdicts: stays on the agent step
	// being wrapped, since that is what internal/agent reads. Also
	// valid as a hook body, where it tolerates the hook's failure the same way.
	Try *Step `yaml:"try,omitempty"`
	// Do runs several steps one after another, as a single plan step.
	//
	// Sequential execution is what a plan does anyway, so the ordering is not
	// the point — the containment is. A hook on the block observes the whole
	// group's outcome, which is the only way to say "roll back if any of these
	// three failed" without repeating the hook on each step or hoisting it to
	// the job. Mirrors Concourse's do: (concourse-ci.org/docs/steps/do/).
	//
	// The block is a container like in_parallel:/race:: it takes no operation
	// fields of its own (no inputs:, run:, image: — those belong on the steps
	// inside), and it carries hooks, when:, try: and to: as any positioned
	// step does.
	Do []Step `yaml:"do,omitempty"`
	// InParallel runs several steps at the same time instead of one after
	// another. See InParallel.
	InParallel *InParallel `yaml:"in_parallel,omitempty"`
	// Race runs several steps at once and keeps whichever finishes
	// successfully first. See Race.
	Race *Race `yaml:"race,omitempty"`
	// Ensemble asks several agents the same question and combines their
	// answers into one verdict to route on. See Ensemble.
	Ensemble *Ensemble `yaml:"ensemble,omitempty"`
	// Approval pauses the plan until a person approves or rejects. See
	// Approval.
	Approval *Approval `yaml:"approval,omitempty"`
	// LoadVar captures a value produced DURING the run — the contents of
	// VarFile, trimmed — into a pipeline var that later steps reference as
	// ((name)). See validateVars.
	LoadVar string `yaml:"load_var,omitempty"`
	// VarFile is the workspace-relative file a load_var: step reads.
	VarFile string `yaml:"file,omitempty"`
	// Passed, on a get step, names upstream jobs this version must ALREADY
	// have succeeded in before this job will run against it. See
	// validatePassed.
	Passed []string `yaml:"passed,omitempty"`
	// Across runs this step once per combination of the listed values,
	// substituting {{ .vars.<name> }} into its fields. Expanded at load, so
	// each cell is an ordinary plan step. See AcrossVar.
	Across []AcrossVar `yaml:"across,omitempty"`
	// Budget caps what every cell of an across: block may spend TOGETHER, and
	// unlike the job and agent ceilings it DEGRADES rather than failing: when
	// the matrix has spent its allowance, no further cell is started, the ones
	// already finished keep their work, and the plan carries on.
	//
	// That difference is the point. A job ceiling is a backstop against a
	// runaway, so failing is right. A runtime fan-out is the one step whose
	// cost nobody could know when they wrote the pipeline — its width is
	// decided mid-run, usually by a model — and there, "review eight of the
	// twelve dimensions and publish" beats "spend the same money and publish
	// nothing".
	//
	// Cells that never started are simply not run: they record nothing, so a
	// rerun with a larger ceiling picks up exactly where this one stopped
	// (finished cells are cached, unstarted ones are not).
	//
	// across: steps only, tokens only. Never hashed, like every other
	// operational limit — adding one must not invalidate a cached cell.
	Budget *Budget `yaml:"budget,omitempty"`
	// MaxInFlight bounds how many across: cells run at once. Unset or 1 keeps
	// the serial declaration-order walk this modifier has always had; anything
	// higher runs that many cells concurrently, and a value at or above the
	// cell count is effectively unbounded. Opt-in, unlike in_parallel:'s
	// limit: where an absent value means unbounded — each default matches the
	// contract its own block already had.
	//
	// Above 1 it requires workspace isolation, for the reason race: does:
	// cells are one step's clones, so they write the same output names into
	// the same working directory, and under the shared strategy concurrent
	// cells are two writers on one file. See validateAcrossConcurrency.
	//
	// NOT hashed, unlike in_parallel:'s limit:/fail_fast:. Those change which
	// steps run at all; this only changes how many run at once, and the cell
	// set is identical at any width — so raising it must not re-run a matrix
	// whose cells are all cached.
	MaxInFlight int `yaml:"max_in_flight,omitempty"`
	// Inputs/Outputs declare which named artifacts a task/agent/put step
	// draws from and (task/agent only) produces. Each name is either a
	// resource fetched by an earlier get step or an output produced by an
	// earlier task/agent step. Both are optional and default to empty: an
	// absent inputs: means "sees nothing declared" (there is no requirement to
	// declare them). When present, an inputs: naming an artifact nothing
	// produces is caught by workspace.ValidateArtifactFlow. Declarations only
	// change what a step physically sees under a top-level workspace: block
	// (see WorkspaceConfig); without one they are a validated contract, not
	// isolation, and are never folded into a node's hash (see internal/merkle).
	// Inputs is a *InputSpec so an absent key (nil) is distinguishable from an
	// explicit empty list — which matters for ResolveTask's override rule (a
	// step's inputs: override its tasks: entry only when declared) — and so put
	// steps can accept the scalar `inputs: all` (every available artifact) in
	// addition to a sequence of names. Invalid on get steps; Outputs is
	// additionally invalid on put steps.
	Inputs  *InputSpec `yaml:"inputs,omitempty"`
	Outputs []string   `yaml:"outputs,omitempty"`
	// Image, on a task or agent step, overrides the referenced task's/
	// agent's Image for this step only (inherit-only: a non-empty step Image
	// always wins, there is no way to force host execution from a step when
	// the task/agent sets one). Invalid on get/put steps — a put's execution
	// image comes from its resource type, and a get has no task/agent to
	// override.
	Image string `yaml:"image,omitempty"`
	// Env, on a task or agent step, overrides the referenced task's/agent's
	// Env for this step only (declared-wins, like Inputs: an explicit empty
	// list means "nothing beyond the baseline", which is distinguishable from
	// absent). Invalid on get/put steps, matching Image: a put's environment
	// comes from its resource type, and a get has no task/agent to override.
	Env []string `yaml:"env,omitempty"`
	// User, on a task or agent step, overrides the referenced task's/agent's
	// User for this step only (non-empty-wins, like Image). Invalid on
	// get/put steps, for the same reason Image is.
	User string `yaml:"user,omitempty"`
	// Network, on a task or agent step, overrides the referenced task's/
	// agent's Network for this step only (non-empty-wins, like Image).
	// Invalid on get/put steps, for the same reason Image is.
	Network string `yaml:"network,omitempty"`
	// Privileged, on a task or agent step, overrides the referenced task's/
	// agent's Privileged for this step only. True-wins, like Image's
	// non-empty-wins: there is no way to force UNprivileged from a step when
	// the task/agent asked for it, matching how every other execution setting
	// inherits. Invalid on get/put steps, for the reason Image is.
	Privileged bool `yaml:"privileged,omitempty"`
	// Limits, on a task or agent step, overrides the referenced task's/agent's
	// container_limits: for this step only (set-wins, like Image).
	Limits *ContainerLimits `yaml:"container_limits,omitempty"`
	// When, on a task/put/agent step, gates whether the step runs at all: an
	// explicit command whose exit code decides (0 runs, nonzero skips). See
	// WhenSpec. Invalid on get steps — a get fans the remainder of the plan
	// out per version, so a conditional get has no coherent meaning.
	When *WhenSpec `yaml:"when,omitempty"`
	// To, on a task/put/verdict-less agent step, routes to another step in the
	// SAME get-segment based on this step's binary outcome: "success" and
	// "failure" are the only keys, and both are reserved. An open map (not a
	// fixed struct) so a later exit-code routing extension needs no new type.
	// Invalid on get steps, hook steps, and any step declaring Verdicts —
	// which carry their own targets. Absent, the plan falls through in
	// declaration order exactly as before. See validateStepTransitions and
	// internal/pipeline's resolveTransition.
	To map[string]string `yaml:"to,omitempty"`
	// MaxVisits caps how many times THIS step may execute in one run. It is
	// required (LoadConfig) whenever any To target routes backward (a target
	// at or before this step's own position within its segment); 0/unset means
	// unbounded, which is only legal when every To target is strictly forward.
	MaxVisits int `yaml:"max_visits,omitempty"`
	// Verdicts, on an agent step, declares the outcome vocabulary the model
	// emits AND where each outcome sends the plan. Its presence turns on
	// verdict mode: internal/agent synthesizes a required `verdict` tool whose
	// enum is exactly these names, and the model must call it. An entry may be
	// a bare name (record the verdict, carry on) or a `name: target` pair
	// (record it and jump); `failure:` is the reserved catch for a step that
	// errored or emitted nothing. Order is significant — it is the tool enum,
	// and an ensemble's decide: any precedence. Agent-only, and mutually
	// exclusive with To. See VerdictRoute.
	Verdicts []VerdictRoute `yaml:"verdicts,omitempty"`
	// ContextPaths lists files whose contents are injected at conversation
	// start as synthetic read_file tool results — the agent sees the file
	// contents as if it had called read_file itself, without consuming a
	// turn. Paths are relative to the step's working directory and confined
	// to its workspace (resolveAgentPath); in practice each file lives
	// inside a declared input, e.g. ["repo/CLAUDE.md"]. Only valid on agent
	// steps. A missing or escaping file fails the step at preparation, before
	// a token is spent; one merely over MaxContextBytes is truncated, with a
	// note saying so.
	ContextPaths []string `yaml:"context_paths,omitempty"`
	// MaxContextBytes overrides the agent's max_context_bytes: for this step
	// only, capping how much of each ContextPaths file is handed over. 0 (the
	// common case) defers to the agent's, which itself falls back to
	// DefaultMaxContextBytes.
	//
	// It belongs here as well as on the agent because context_paths: is itself
	// a step-level field: two steps sharing one agents: entry routinely hand
	// it different evidence — a 400KB diff to the reviewer, a small manifest
	// to the gatekeeper — and without a step-level ceiling the only way to
	// give them different ones is to duplicate the whole agent under a second
	// name for the sake of one number. (context_window: has no step spelling
	// for the mirror-image reason: it describes the MODEL, and the model is
	// the agent's.) Operational, like the agent's, and likewise never hashed.
	MaxContextBytes int `yaml:"max_context_bytes,omitempty"`
	// Hooks are the step's on_success/on_failure/on_error/on_abort/ensure
	// reaction steps (see Hooks). Inlined so they sit alongside the step's
	// own fields in YAML.
	Hooks Hooks `yaml:",inline"`
	// Assert, on a task/agent step, checks the step's captured output/exit
	// code (see Assert). A matching assert makes a non-zero-exit task a
	// success; a mismatch fails the step. Invalid on get/put steps.
	Assert *Assert `yaml:"assert,omitempty"`
	// Label is COMPUTED at expansion (never written in YAML): the name this
	// step is KNOWN BY, when that differs from the name it is RESOLVED by.
	//
	// task:/agent:/put: each do two jobs — they identify the step (routing
	// target, recorded node, assert.execution) and they look something up
	// (FindTask, FindAgent, the resource). An across: matrix needs its cells to
	// be distinguishable, so it used to append coordinates to the very field
	// the lookup keys on: a cell over a shared tasks: entry then failed with
	// `no task named "shared [shard=b]"`, and an agent cell could not be
	// renamed at all, since FindAgent has no inline escape the way ResolveTask
	// does. So every agent cell of a matrix answered to one name, which is why
	// context: is still rejected there.
	//
	// Label carries the identity half, leaving the lookup half alone. Empty on
	// an ordinary step, where the two names are the same thing — see stepName.
	Label string `yaml:"-"`
	// OutputSubdir is COMPUTED at expansion (never written in YAML): this
	// cell's coordinates as a path — one segment per axis value, declaration
	// order ("alpha/fast"). A matrix that declares outputs: captures each
	// cell's outputs under it (findings/alpha/fast/...), which is what lets N
	// cells share one declared output name without clobbering each other; see
	// CollectedOutputMapping. Empty on an ordinary step, and ignored on a cell
	// that declares no outputs.
	OutputSubdir string `yaml:"-"`
	// Context, on an agent or task step, opts into reading named earlier
	// steps' decisions: `context: { from: { <step>: verdict|note|full } }`.
	// Never a hook — see validateContextSteps and contextfrom.go.
	Context *ContextSpec `yaml:"context,omitempty"`
	// NoteRequired is COMPUTED at load (never written in YAML): some other
	// step declared context: { from: { <this step>: note|full } }, so this
	// step's verdict note stops being optional. The obligation lives here
	// rather than on the reader because internal/agent builds the sender's
	// tool set and never sees the reader. See validateContextFrom.
	NoteRequired bool `yaml:"-"`
	// InputMapping/OutputMapping rename a task step's declared inputs/outputs
	// onto plan-artifact names, mirroring Concourse's input_mapping/
	// output_mapping (see docs/conformance.md;
	// TestRunJobIsolatedGetAliasMappingAndPutAll in
	// workspace_integration_test.go; Concourse doc: concourse-ci.org/docs/
	// steps/task/): each entry is {task-config-name: plan-artifact-name}, so
	// a reusable tasks: entry with pinned input names can be pointed at
	// whatever a job actually fetched/produced without editing the task. Keys
	// must be a subset of the resolved task's declared inputs:/outputs:. Task
	// steps only, and only meaningful under a workspace: block (mapping renames
	// a materialized directory, which the shared single directory can't do) —
	// both are load-time errors otherwise. Absent/empty leaves names unmapped.
	InputMapping  map[string]string `yaml:"input_mapping,omitempty"`
	OutputMapping map[string]string `yaml:"output_mapping,omitempty"`
	// Resource, on a get step, names the resource to fetch when it differs from
	// the step's own name: the fetched artifact (and the directory, step name,
	// and to: target) is Get, while the resource whose check/in runs is
	// Resource — mirroring Concourse's get.resource, including that two
	// aliased get steps for the same underlying resource share one version
	// history rather than tracking separately (see docs/conformance.md;
	// TestResourcesAndAffectedJobsResolveGetAlias in
	// internal/trigger/trigger_test.go; Concourse doc: concourse-ci.org/docs/
	// steps/get/). This lets one resource appear under a task-friendly name,
	// or twice in a plan under two names. Empty (the default) means the
	// resource name equals Get. Get steps only.
	Resource string `yaml:"resource,omitempty"`
	// Line is the step's source line in the pipeline file, filled in after
	// decoding (see stampLines) so a validation error can point at a place in
	// the file. Never written in YAML and never hashed.
	Line int `yaml:"-"`
}

// InputSpec is a step's inputs: declaration. It is a distinct type rather than
// a plain []string for two reasons: an absent inputs: key (nil *InputSpec)
// must be distinguishable from an explicit empty list (so ResolveTask can apply
// a step's inputs: over its tasks: entry only when the step actually declared
// one), and put steps accept the scalar `inputs: all` — every available
// artifact — in addition to a sequence of names. Names holds the explicit
// list; All is set only by the scalar form and is valid on put steps only.
type InputSpec struct {
	Names []string
	All   bool
}

// UnmarshalYAML decodes an InputSpec from either the scalar `all` or a
// sequence of artifact names. Any other scalar is rejected here so a typo like
// `inputs: repo` (meaning `inputs: [repo]`) fails loudly rather than silently
// parsing as a zero-name declaration.
func (in *InputSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias/mapping kinds that can't appear here
	case yaml.ScalarNode:
		var s string

		err := node.Decode(&s)
		if err != nil {
			return fmt.Errorf("inputs: %w", err)
		}

		if s != "all" {
			return fmt.Errorf("inputs: scalar value must be \"all\" (or use a sequence of names), got %q", s)
		}

		in.All = true

		return nil
	case yaml.SequenceNode:
		return node.Decode(&in.Names) //nolint:wrapcheck // yaml.v3 error is already descriptive
	default:
		return fmt.Errorf("inputs at line %d must be a sequence of names or the scalar \"all\"", node.Line)
	}
}

// names returns the explicit input names, nil-safe, so a caller holding a
// possibly-absent inputs: doesn't have to nil-check before reading it.
func (in *InputSpec) names() []string {
	if in == nil {
		return nil
	}

	return in.Names
}

// validateTaskInputsAll rejects `inputs: all` on a tasks: entry. The scalar
// means "every artifact available at this point", which is a property of the
// job doing the calling — a reusable task that declared it would see something
// different in every job, which is the opposite of reusable. It stays a put
// step's escape hatch (see InputSpec).
func (c *Config) validateTaskInputsAll() error {
	for _, task := range c.Tasks {
		if task.Inputs != nil && task.Inputs.All {
			return fmt.Errorf("task %q: inputs: all is only valid on put steps; list the names this task needs", task.Name)
		}
	}

	return nil
}

// Inputs constructs a declared *InputSpec from an explicit name list —
// Inputs() declares the empty "starts from nothing" form, Inputs("a", "b") a
// named list. It exists so callers building a Step programmatically (tests,
// and any future config synthesis) get the same declared-vs-absent distinction
// yaml.v3 gives a decoded pipeline.
func Inputs(names ...string) *InputSpec {
	if names == nil {
		names = []string{}
	}

	return &InputSpec{Names: names}
}

// InputsDeclared reports whether the step carried an inputs: key at all
// (nil vs present, including inputs: []). ResolveTask reads this so a step's
// inputs: override its tasks: entry only when the step actually declared one.
func (s Step) InputsDeclared() bool { return s.Inputs != nil }

// InputNames returns the explicit input names (nil for an absent or all-form
// inputs:). It is the []string every existing input consumer expects.
func (s Step) InputNames() []string {
	if s.Inputs == nil {
		return nil
	}

	return s.Inputs.Names
}

// InputsAll reports whether the step declared `inputs: all` (put steps only).
func (s Step) InputsAll() bool { return s.Inputs != nil && s.Inputs.All }

// GetResourceName is the name of the resource a get step fetches: Resource
// when set (get: aliases the resource under a different name), else Get itself.
// The fetched artifact, its directory, and the step's routing name are always
// Get; only the resource whose check/in runs is GetResourceName.
func (s Step) GetResourceName() string {
	if s.Resource != "" {
		return s.Resource
	}

	return s.Get
}

// FileRef is an agent step's prompt_file: — the text of the model's prompt
// for this step, loaded from a file rather than written inline. A scalar path
// is relative to the pipeline YAML's own directory and is resolved at load
// time, exactly like every other *_file field (see LoadConfig's
// resolveFileIncludes). A mapping {artifact, path} instead names a file
// inside an artifact this step declares in its inputs: — resolved at run
// time, once that artifact is fetched, since merkle.PlanChains hashes every
// step before any get's `in` has run (see internal/merkle's planGetStep) and
// so cannot see the file at plan time.
type FileRef struct {
	Path     string
	Artifact string
}

// UnmarshalYAML decodes a FileRef from either a scalar (a pipeline-relative
// path) or a mapping ({artifact, path}) YAML node — the same scalar-or-mapping
// idiom as WhenSpec/FixSpec.
func (f *FileRef) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&f.Path) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		err := rejectUnknownKeys(value, "prompt_file", "artifact", "path")
		if err != nil {
			return err
		}

		var m struct {
			Artifact string `yaml:"artifact"`
			Path     string `yaml:"path"`
		}

		err = value.Decode(&m)
		if err != nil {
			return fmt.Errorf("prompt_file: %w", err)
		}

		if m.Artifact == "" || m.Path == "" {
			return fmt.Errorf("prompt_file at line %d: an {artifact, path} mapping requires both fields", value.Line)
		}

		f.Artifact, f.Path = m.Artifact, m.Path

		return nil
	default:
		return fmt.Errorf("prompt_file at line %d must be a path or an {artifact, path} mapping", value.Line)
	}
}

// Deferred reports whether f names a run-time artifact file (the {artifact,
// path} mapping form) rather than a load-time, pipeline-relative one.
func (f *FileRef) Deferred() bool {
	return f != nil && f.Artifact != ""
}

// resolveGetReference resolves a get step's resource, unless it aliases one —
// an alias is validateGetResource's to check.
func (c *Config) resolveGetReference(step *Step) error {
	if step.Resource != "" {
		return nil
	}

	_, err := c.FindResource(step.Get)

	return err
}

// branchesOf returns a concurrent block's branches keyed by which kind of
// block they belong to, or nothing for an ordinary step. It exists so the
// walkers below treat in_parallel: and race: identically — they differ in what
// their branches' outcomes MEAN, never in the fact that they have branches,
// and a walker that knew about one but not the other is precisely the silent
// gap this codebase has shipped before.
func branchesOf(step *Step) map[string][]Step {
	//kindswitch:ignore only the container kinds have branches; the leaf kinds are the point of the default
	switch {
	case step.InParallel != nil:
		return map[string][]Step{"in_parallel": step.InParallel.Steps}
	case step.Race != nil:
		return map[string][]Step{"race": step.Race.Steps}
	case step.Ensemble != nil:
		return map[string][]Step{"ensemble": step.Ensemble.Agents}
	default:
		return nil
	}
}

// resolveBranchReferences resolves every branch of an in_parallel: block. A
// block references nothing itself; its branches are ordinary steps and
// reference exactly what they always did.
func (c *Config) resolveBranchReferences(steps []Step) error {
	for i := range steps {
		err := c.resolveStepReference(&steps[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// ReferencesEntities reports whether a step of this kind names something the
// pipeline has to resolve — a resource, a task, an agent, or branches that do.
func (k StepKind) ReferencesEntities() bool {
	return k != StepKindLoadVar && k != StepKindApproval
}

// StepKind is which of Get/Task/Put/Agent a Step is. See Step.Kind.
type StepKind string

// The StepKind values, one per Step field Kind can resolve to.
const (
	StepKindGet   StepKind = "get"
	StepKindTask  StepKind = "task"
	StepKindPut   StepKind = "put"
	StepKindAgent StepKind = "agent"
	StepKindTry   StepKind = "try"
	// StepKindInParallel is a block of steps that run concurrently. Adding it
	// to this table is what makes `go run ./tools/kindswitch ./...` demand an
	// answer from every dispatch site — the tagged ones via `exhaustive`, the
	// tagless ones via that analyzer. Story 001 added a kind and shipped ten
	// defects, six of them a dispatch site that silently did nothing for it.
	StepKindInParallel StepKind = "in_parallel"
	// StepKindRace is a block whose first successful branch wins.
	StepKindRace StepKind = "race"
	// StepKindEnsemble is a block whose members vote on a verdict.
	StepKindEnsemble StepKind = "ensemble"
	// StepKindLoadVar captures a run-time value into a pipeline var.
	StepKindLoadVar StepKind = "load_var"
	// StepKindApproval waits for a human decision.
	StepKindApproval StepKind = "approval"
	// StepKindDo is a block of steps that run one after another AS ONE STEP.
	// Its value is entirely in that last part: a hook on the block covers
	// every step inside it, which is the one thing a plain run of sibling
	// steps cannot express (see Step.Do).
	StepKindDo StepKind = "do"
)

// Kind reports which single kind of step s is. ok is false when zero, or
// more than one, of Get/Task/Put/Agent is set — a malformed step every call
// site should reject the same way, rather than each silently picking
// whichever field its own historical check order happened to test first.
func (s Step) Kind() (kind StepKind, ok bool) {
	for _, candidate := range [...]struct {
		kind StepKind
		set  bool
	}{
		{StepKindGet, s.Get != ""},
		{StepKindTask, s.Task != ""},
		{StepKindPut, s.Put != ""},
		{StepKindAgent, s.Agent != ""},
		{StepKindTry, s.Try != nil},
		{StepKindInParallel, s.InParallel != nil},
		{StepKindRace, s.Race != nil},
		{StepKindEnsemble, s.Ensemble != nil},
		{StepKindLoadVar, s.LoadVar != ""},
		{StepKindApproval, s.Approval != nil},
		{StepKindDo, s.Do != nil},
	} {
		if !candidate.set {
			continue
		}

		if ok {
			return "", false // a second kind field was set — reject, don't silently keep the first
		}

		kind, ok = candidate.kind, true
	}

	return kind, ok
}

// validateStepKinds rejects a step that names no kind, or more than one.
//
// Step.Kind already answers this, but until now only validateHookStep and
// validateStepAssert asked: a plan step setting both task: and agent: loaded
// cleanly and failed mid-run, after earlier steps had already executed. A
// malformed step is a typo, and a typo should cost a load, not a build.
func (c *Config) validateStepKinds() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			_, ok := step.Kind()
			if ok {
				return nil
			}

			set := step.kindFieldsSet()
			if len(set) == 0 {
				return fmt.Errorf("%s: step names no kind, set one of get/task/put/agent", label)
			}

			return fmt.Errorf("%s: step sets %s, but a step is exactly one of get/task/put/agent",
				label, strings.Join(set, " and "))
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// kindFieldsSet names the kind-selecting fields this step sets, in schema
// order, for the "sets task and agent" half of validateStepKinds' message.
func (s Step) kindFieldsSet() []string {
	set := make([]string, 0, 5)

	for _, candidate := range [...]struct {
		name  string
		value string
	}{
		{"get", s.Get},
		{"task", s.Task},
		{"put", s.Put},
		{"agent", s.Agent},
	} {
		if candidate.value != "" {
			set = append(set, candidate.name)
		}
	}

	if s.Try != nil {
		set = append(set, "try")
	}

	return set
}

// validateStepReferences rejects a step naming a resource, agent, or tasks:
// entry that the pipeline does not define.
//
// A misspelled name is the most common way to break an otherwise well-formed
// pipeline, and it used to survive load: only a get step's resource: alias was
// checked, so `agent: reviwer` or `put: relase` failed partway through a run,
// after earlier steps had already done their work. Checking every reference up
// front makes a typo cost a load rather than a build.
func (c *Config) validateStepReferences() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			err := c.resolveStepReference(step)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// resolveStepReference looks up whatever a step names, reporting the lookup
// error unchanged. A step whose kind is malformed resolves to nothing —
// validateStepKinds reports that on its own rather than having every caller
// repeat it.
func (c *Config) resolveStepReference(step *Step) error {
	// A load_var: names a file, not a pipeline entity, so there is nothing to
	// resolve against resources, tasks, or agents — same as a malformed step,
	// whose kind another validator reports.
	// A load_var: names a file and an approval: names a person; neither
	// references a pipeline entity, same as a malformed step whose kind
	// another validator reports.
	kind, ok := step.Kind()
	if !ok || !kind.ReferencesEntities() {
		return nil
	}

	var err error

	switch kind { //nolint:exhaustive // StepKindLoadVar returned above
	case StepKindGet:
		return c.resolveGetReference(step)
	case StepKindPut:
		_, err = c.FindResource(step.Put)
	case StepKindAgent:
		_, err = c.FindAgent(step.Agent)
	case StepKindTry:
		err = c.resolveStepReference(step.Try)
	case StepKindInParallel, StepKindRace, StepKindEnsemble:
		for _, branches := range branchesOf(step) {
			err = c.resolveBranchReferences(branches)
		}
	case StepKindTask:
		return c.resolveTaskReference(step)
	}

	return err
}

// resolveTaskReference resolves a task step's tasks: entry. An inline task —
// one carrying its own run: — never consults tasks: at all.
func (c *Config) resolveTaskReference(step *Step) error {
	if step.Run != "" {
		return nil
	}

	_, err := c.FindTask(step.Task)

	return err
}

// validateStepFieldPlacement rejects the three kind-specific fields that had
// no placement check: trigger: and version: (get-only) and params:
// (get/put-only).
//
// Every other kind-specific field already errors when written on the wrong
// kind. These three were silently ignored instead, so `trigger: true` on a
// task step — a plausible reading of "run this when something changes" —
// looked accepted and did nothing.
//
// params: is valid on BOTH resource-facing kinds, matching Concourse: a put's
// params reach out:, a get's reach in: (concourse-ci.org/docs/steps/get/).
// A get's params are how a resource is told HOW to fetch — git's depth: and
// submodules:, s3's unpack: — which is per-get rather than per-resource, and
// so cannot live in source: without forcing two resources for two fetch
// styles of one repository.
func (c *Config) validateStepFieldPlacement() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(checkStepFieldPlacement)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkStepFieldPlacement is validateStepFieldPlacement's per-step half: the
// fields that are only meaningful on one kind.
func checkStepFieldPlacement(label string, step *Step) error {
	isGet, isPut := step.Get != "", step.Put != ""

	switch {
	case step.Trigger && !isGet:
		return fmt.Errorf("%s: trigger is only valid on get steps", label)
	case step.Version != nil && !isGet:
		return fmt.Errorf("%s: version is only valid on get steps", label)
	case step.Params != nil && !isGet && !isPut:
		return fmt.Errorf("%s: params is only valid on get and put steps", label)
	default:
		return checkImplicitGetFields(label, step, isPut)
	}
}

// checkImplicitGetFields places the two switches on the implicit get a put
// runs, and rejects the combination where one cancels the other.
func checkImplicitGetFields(label string, step *Step, isPut bool) error {
	switch {
	case step.GetParams != nil && !isPut:
		return fmt.Errorf("%s: get_params is only valid on put steps (it configures the implicit get a put runs; a get step spells its own fetch options params:)", label)
	case step.NoGet && !isPut:
		return fmt.Errorf("%s: no_get is only valid on put steps (it skips the implicit get a put runs)", label)
	case step.NoGet && step.GetParams != nil:
		return fmt.Errorf("%s: get_params is set alongside no_get: true, but no_get skips the very fetch get_params configures — remove one", label)
	default:
		return nil
	}
}

func visitStepTree(label string, step *Step, fn func(label string, step *Step) error) error {
	err := fn(label, step)
	if err != nil {
		return err
	}

	if step.Try != nil {
		err = visitStepTree(label+" (try)", step.Try, fn)
		if err != nil {
			return err
		}
	}

	// Descend into a concurrent block's branches. Without this every
	// validator in this package would silently stop at the block, and a
	// branch could carry anything at all.
	for kind, branches := range branchesOf(step) {
		for i := range branches {
			err = visitStepTree(fmt.Sprintf("%s (%s branch %d)", label, kind, i), &branches[i], fn)
			if err != nil {
				return err
			}
		}
	}

	// A do: block's children are visited too, but deliberately NOT through
	// branchesOf: that function means "concurrent branches", and its other
	// callers (context scoping, handoff-note fan-in/broadcast) attach
	// concurrency semantics to whatever it returns. A do: block is sequential,
	// so its children behave like ordinary consecutive plan steps — which is
	// the entire reason those semantics must not reach them.
	for i := range step.Do {
		err = visitStepTree(fmt.Sprintf("%s (do step %d)", label, i), &step.Do[i], fn)
		if err != nil {
			return err
		}
	}

	return step.Hooks.Each(func(name string, hook *Step) error {
		return visitStepTree(fmt.Sprintf("%s (%s hook)", label, name), hook, fn)
	})
}

// Unwrap returns the step a try: chain ultimately wraps — s itself when s is
// not a try wrapper. Callers that care about what a plan step actually RUNS
// (which agent, which verdicts:) go through this, since a try:
// wrapper carries none of those fields itself.
func (s Step) Unwrap() Step {
	return *unwrapStep(&s)
}

// unwrapStep is Unwrap for a caller that needs to MUTATE what it finds — see
// stampNoteObligations, which stamps NoteRequired onto the agent step a try:
// wraps rather than onto the wrapper the runtime never hands to an agent. A
// free function rather than a second method, so Step keeps a single receiver
// kind (.golangci.yml's recvcheck).
func unwrapStep(s *Step) *Step {
	for s.Try != nil {
		s = s.Try
	}

	return s
}

// validateTrySteps rejects malformed try wrappers: a bare try: (nil inner
// step), try: wrapped around a get step, try: that also sets another kind
// field, or a try: whose inner step has no recognized kind. It also enforces
// the division of fields between the wrapper and what it wraps — see
// validateWrappedStepFields.
func (c *Config) validateTrySteps() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Try == nil {
				return nil
			}

			innerKind, ok := step.Try.Kind()
			if !ok {
				return fmt.Errorf("%s: try: wraps an unrecognized or empty step", label)
			}
			if innerKind == StepKindGet {
				return fmt.Errorf("%s: try: cannot wrap a get step", label)
			}

			fields := step.kindFieldsSet()
			if len(fields) > 1 { // "try" + something else
				return fmt.Errorf("%s: try: is a wrapper — do not also set get/task/put/agent", label)
			}

			return validateWrappedStepFields(label, step.Try)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateWrappedStepFields rejects the fields that are dead config on the step
// inside a try:, because only the wrapper occupies a position in the plan.
//
// Routing is resolved against the plan step (internal/pipeline's applyRouting
// never sees the wrapped step), so a to:/max_visits: written one level too deep
// used to load fine and then silently never fire — the plan just fell through
// past a failure the author believed they had routed.
func validateWrappedStepFields(label string, inner *Step) error {
	switch {
	case inner.To != nil:
		return fmt.Errorf("%s: to: belongs on the try: step, not the step it wraps (only the wrapper has a position in the plan)", label)
	case inner.MaxVisits != 0:
		return fmt.Errorf("%s: max_visits belongs on the try: step, not the step it wraps (only the wrapper has a position in the plan)", label)
	default:
		return nil
	}
}
