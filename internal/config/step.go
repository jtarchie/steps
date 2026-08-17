package config

// A plan step: the flat get/task/put/agent union, its inputs: declaration,
// its prompt_file: reference, and the walk over a step and its hook tree.

import (
	"fmt"

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
	// Attempts is how many times this step's work is tried before it fails.
	// Unset takes 1 for task/get/put steps and defaultAgentAttempts for agent
	// steps (where a retry is a transport concern, not a re-run). An explicit
	// 0 is a load error — see dials.go for why attempts: sits outside the
	// zero-means-no-limit convention the other dials follow.
	Attempts *int `yaml:"attempts,omitempty"`
	// MaxTurns overrides the agent entry's max_turns for this one step:
	// tool-calling turns per attempt. Unset inherits the agent's value (which
	// itself defaults to defaultMaxAgentTurns); an explicit 0 removes the cap
	// for this step alone. Agent steps only — one long-horizon step can buy
	// more turns without every step of the same agent paying for them.
	MaxTurns *int `yaml:"max_turns,omitempty"`
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// means no timeout on a task/get/put step, and the agent entry's timeout:
	// — or the 30-minute package default — on an agent step, which is the one
	// step kind that gets a deadline it never asked for. "0" there is how a
	// long-horizon agent step says it wants none; on any other step kind it
	// is a load error, since omitting the field already says that.
	//
	// Valid on all step kinds (a get step's timeout bounds both its check and
	// in commands); for task/put steps it overrides the referenced
	// task/agent's Timeout.
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
	// Cells are one step's clones — same output names, same paths — and each
	// materializes its own directory, so concurrent cells never collide.
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
	// Volatile marks a step whose result must never be reused: it reads
	// something the pipeline never declared — a clock, a network endpoint, a
	// path outside its inputs — so a recorded answer is a stale one. Off by
	// default, because a step's declared inputs are what identify its work,
	// which is the contract a task's run: has always had. Valid only on task
	// and agent steps: they are the two kinds the step cache reuses at all.
	Volatile bool `yaml:"volatile,omitempty"`
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
	// only, capping how much of each ContextPaths file is handed over. Unset
	// (the common case) defers to the agent's, which itself falls back to
	// DefaultMaxContextBytes; an explicit 0 hands the files over whole.
	//
	// It belongs here as well as on the agent because context_paths: is itself
	// a step-level field: two steps sharing one agents: entry routinely hand
	// it different evidence — a 400KB diff to the reviewer, a small manifest
	// to the gatekeeper — and without a step-level ceiling the only way to
	// give them different ones is to duplicate the whole agent under a second
	// name for the sake of one number. (context_window: has no step spelling
	// for the mirror-image reason: it describes the MODEL, and the model is
	// the agent's.) Operational, like the agent's, and likewise never hashed.
	MaxContextBytes *int `yaml:"max_context_bytes,omitempty"`
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
	// steps only (a load-time error elsewhere); mapping renames a materialized
	// directory. Absent/empty leaves names unmapped.
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

// VersionEvery reports whether this get declared `version: every` — the one
// mode that runs the rest of the plan once per version. Every other spelling
// (unset, "latest", a pinned mapping) resolves to a single version; see
// resource.VersionMode, which reads this.
func (s Step) VersionEvery() bool {
	mode, ok := s.Version.(string)

	return ok && mode == "every"
}

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
