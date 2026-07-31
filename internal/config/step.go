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
	// Put names a resource to run its out command against; Params are
	// passed through to the out command as {{ params.x }}.
	Put    string         `yaml:"put,omitempty"`
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
	Attempts   int        `yaml:"attempts,omitempty"`
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// (default) means no timeout. Valid on all step kinds (a get step's
	// timeout bounds both its check and in commands); for task/put steps it
	// overrides the referenced task/agent's Timeout.
	Timeout string `yaml:"timeout,omitempty"`
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
	// When, on a task/put/agent step, gates whether the step runs at all: an
	// explicit command whose exit code decides (0 runs, nonzero skips). See
	// WhenSpec. Invalid on get steps — a get fans the remainder of the plan
	// out per version, so a conditional get has no coherent meaning.
	When *WhenSpec `yaml:"when,omitempty"`
	// To, on a task/put/agent step, routes to another step in the SAME
	// get-segment based on this step's outcome, keyed by outcome name:
	// "success"/"failure" for a task/put/verdict-less agent, or a verdict name
	// for an agent that declares Verdicts. An open map (not a fixed struct) so
	// verdict keys — and, later, exit-code keys — need no new type. "success"
	// and "failure" are reserved keys. Invalid on get steps and hook steps.
	// Absent, the plan falls through in declaration order exactly as before.
	// See validateStepTransitions and internal/pipeline's resolveTransition.
	To map[string]string `yaml:"to,omitempty"`
	// MaxVisits caps how many times THIS step may execute in one run. It is
	// required (LoadConfig) whenever any To target routes backward (a target
	// at or before this step's own position within its segment); 0/unset means
	// unbounded, which is only legal when every To target is strictly forward.
	MaxVisits int `yaml:"max_visits,omitempty"`
	// Verdicts, on an agent step, declares the outcome vocabulary the model
	// emits. Its presence turns on verdict mode: internal/agent synthesizes a
	// required `verdict` tool whose enum is exactly these, the model must call
	// it, and To routes on the chosen value. Every declared verdict must have a
	// To entry, and no verdict may be named with a reserved key. Agent-only.
	Verdicts []string `yaml:"verdicts,omitempty"`
	// ContextPaths lists files whose contents are injected at conversation
	// start as synthetic read_file tool results — the agent sees the file
	// contents as if it had called read_file itself, without consuming a
	// turn. Paths are relative to the step's working directory and confined
	// to its workspace (resolveAgentPath); in practice each file lives
	// inside a declared input, e.g. ["repo/CLAUDE.md"]. Only valid on agent
	// steps. A missing, escaping, or over-100KB file fails the step at
	// preparation, before a token is spent.
	ContextPaths []string `yaml:"context_paths,omitempty"`
	// Hooks are the step's on_success/on_failure/on_error/on_abort/ensure
	// reaction steps (see Hooks). Inlined so they sit alongside the step's
	// own fields in YAML.
	Hooks Hooks `yaml:",inline"`
	// Assert, on a task/agent step, checks the step's captured output/exit
	// code (see Assert). A matching assert makes a non-zero-exit task a
	// success; a mismatch fails the step. Invalid on get/put steps.
	Assert *Assert `yaml:"assert,omitempty"`
	// Handoff, on an agent step, opts into transition context: when this step
	// is entered via a to:/verdicts: transition, it receives a machine-
	// assembled <transition_context> block (see HandoffSpec.Context) appended
	// to its prompt and/or a synthesized previous_run tool (see
	// HandoffSpec.Tool) it can call to pull the routed-from agent's recorded
	// run on demand. Agent-only, and only valid on a step that is the target
	// of at least one to: route within its own get-segment — see
	// validateHandoffSteps. On the step's first/unrouted execution, no
	// context block is appended and previous_run (if granted) answers "no
	// previous run" as data.
	Handoff *HandoffSpec `yaml:"handoff,omitempty"`
	// HandoffNote, on an agent step, requires the step to write a handoff note
	// before its conversation may end: a synthesized write_handoff tool (see
	// internal/agent) with a fixed three-field form, rendered to
	// handoff/<step>.md in the build workspace and injected into the next
	// agent step's conversation at start. It is the FORWARD counterpart to
	// Handoff, which carries context BACKWARD along a to:/verdicts: route.
	//
	// Agent-only, never on a hook, and only valid when a later agent step
	// exists in the same get-segment to receive it — see
	// validateHandoffNoteSteps, which also resolves HandoffNoteFrom.
	HandoffNote bool `yaml:"handoff_note,omitempty"`
	// HandoffNoteFrom is COMPUTED at load (never written in YAML): the name of
	// the nearest preceding agent step in this step's get-segment that
	// declares handoff_note. "" when no such step exists — the common case,
	// and the step then receives no note. Resolving the receiver statically
	// rather than carrying it through internal/pipeline is what makes note
	// delivery automatically idempotent: every dispatch of this step, first
	// entry or a to:-driven redo, re-reads whatever note is on disk, so a
	// redo always sees the newest one.
	HandoffNoteFrom string `yaml:"-"`
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
// idiom as WhenSpec/FixSpec/HandoffSpec.
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

// StepKind is which of Get/Task/Put/Agent a Step is. See Step.Kind.
type StepKind string

// The StepKind values, one per Step field Kind can resolve to.
const (
	StepKindGet   StepKind = "get"
	StepKindTask  StepKind = "task"
	StepKindPut   StepKind = "put"
	StepKindAgent StepKind = "agent"
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

func visitStepTree(label string, step *Step, fn func(label string, step *Step) error) error {
	err := fn(label, step)
	if err != nil {
		return err
	}

	return step.Hooks.Each(func(name string, hook *Step) error {
		return visitStepTree(fmt.Sprintf("%s (%s hook)", label, name), hook, fn)
	})
}
