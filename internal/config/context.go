package config

// A step's context: — the run-scoped key/value store an agent step may write
// with the synthesized set_context tool.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ContextSpec is a step's context: — what the step does with the run's shared
// key/value store.
//
// One field today. Writing is opt-in because it adds a tool to the model's
// grant, and a grant nobody asked for is the thing this codebase does not do.
// Reading is deliberately NOT a field here: it arrives as an automatic recap
// that every agent step gets, so a step that opts into writing is a reader
// too without saying so twice.
//
// A scalar `context: write` means {write: true}. In the mapping form every
// field means exactly what it says and defaults to off — the rule HandoffSpec
// arrived at the hard way, after an implicit default made one field quietly
// enable another. Setting fidelity: does not enable write:, and setting
// write: does not change the recap.
type ContextSpec struct {
	Write bool
	// Fidelity is how much of the recorded context this step is shown. ""
	// means "not stated here" and falls through to defaults.context.fidelity
	// and then to FidelityCompact — see ResolveContextFidelity.
	Fidelity ContextFidelity
	// From is what this step reads from named earlier steps: their verdict,
	// and optionally the note and response that came with it. Declared on the
	// READER — nothing arrives unasked — and the demand is what makes the
	// sender's note required. See contextfrom.go.
	From map[string]FromLevel
	// Qualify, on an across: step, says the matrix records PER-CELL facts:
	// each cell writes to a scope only it touches, and the join merges them
	// under keys naming the cell (`reviewer [dim=api].finding`) instead of the
	// cells overwriting one another under the plain key.
	//
	// It exists to take that decision away from max_in_flight:. Concurrent
	// cells have to be scoped — two writers on one key produce neither cell's
	// value — so scheduling used to decide the KEY NAMES a downstream step
	// reads, and adding max_in_flight: silently reshaped a data contract.
	// Stated here it is a property of the matrix, so the serial and concurrent
	// spellings of one pipeline agree and max_in_flight: is safe to add and
	// remove — which is the point of it not being hashed.
	//
	// max_in_flight > 1 therefore REQUIRES it rather than implying it: a load
	// error naming the fix, instead of a rename nothing reports.
	Qualify bool
}

// ContextFidelity is how detailed a step's context recap is.
//
// There is deliberately no "share the whole prior conversation" rung. Agent
// steps are hermetic here — each is a fresh conversation — so every level is
// a rendered recap of the recorded facts, differing only in how much of each
// one survives.
type ContextFidelity string

const (
	// FidelityOff delivers no recap at all: the complete opt-out, for a step
	// that should answer from its prompt and nothing else.
	FidelityOff ContextFidelity = "off"
	// FidelityTruncate delivers the key names only. Enough for a step to know
	// what has been established and ask for it, without spending context on
	// values it may not need.
	FidelityTruncate ContextFidelity = "truncate"
	// FidelityCompact delivers each key with its value shortened. The default:
	// the whole point of recording a fact is that the next step reads it, and
	// a key list alone would make every step re-derive the values.
	FidelityCompact ContextFidelity = "compact"
	// FidelitySummary delivers each key with its value in full.
	FidelitySummary ContextFidelity = "summary"
)

// contextFidelities is the vocabulary, in ascending detail — the order the
// error message lists them in, so it reads as a ladder rather than a set.
var contextFidelities = []ContextFidelity{ //nolint:gochecknoglobals // static, read-only vocabulary
	FidelityOff, FidelityTruncate, FidelityCompact, FidelitySummary,
}

// Valid reports whether f is one of the known levels.
func (f ContextFidelity) Valid() bool {
	return slices.Contains(contextFidelities, f)
}

// validateFidelity checks a stated fidelity, naming the vocabulary (and a
// near-miss, when there is one) rather than just refusing.
func validateFidelity(field string, f ContextFidelity, line int) error {
	if f == "" || f.Valid() {
		return nil
	}

	known := make([]string, 0, len(contextFidelities))
	for _, level := range contextFidelities {
		known = append(known, string(level))
	}

	return fmt.Errorf("%s at line %d: unknown fidelity %q%s (known: %s)",
		field, line, f, suggestion(string(f), known), strings.Join(known, ", "))
}

// defaultFidelity reads defaults.context.fidelity, or "" when the pipeline
// declares no defaults: block at all. Both levels are optional pointers, so
// reading through them without this is a nil dereference on the very common
// pipeline that sets neither.
func (c *Config) defaultFidelity() ContextFidelity {
	if c.Defaults == nil || c.Defaults.Context == nil {
		return ""
	}

	return c.Defaults.Context.Fidelity
}

// ResolveContextFidelity reports how much recap a step is shown: the step's
// own fidelity if it stated one, then defaults.context.fidelity, then
// FidelityCompact. First match wins.
func (c *Config) ResolveContextFidelity(step Step) ContextFidelity {
	if step.Context != nil && step.Context.Fidelity != "" {
		return step.Context.Fidelity
	}

	if fidelity := c.defaultFidelity(); fidelity != "" {
		return fidelity
	}

	return FidelityCompact
}

// contextWriteScalar is the only scalar context: accepts. Spelled as a word
// rather than a boolean because `context: true` would not say true to WHAT,
// and the mapping form is where more switches will land.
const contextWriteScalar = "write"

// UnmarshalYAML decodes a ContextSpec from either a scalar (context: write)
// or a mapping ({write: true}) YAML node — the same scalar-or-mapping idiom as
// WhenSpec/FixSpec/HandoffSpec.
func (c *ContextSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		var mode string

		err := value.Decode(&mode)
		if err != nil {
			return fmt.Errorf("step context: %w", err)
		}

		if mode != contextWriteScalar {
			return fmt.Errorf("step context at line %d: unknown mode %q (did you mean %q?)", value.Line, mode, contextWriteScalar)
		}

		c.Write = true

		return nil
	case yaml.MappingNode:
		return c.unmarshalMapping(value)
	default:
		return fmt.Errorf("step context at line %d must be %q or a {write, fidelity, qualify} mapping", value.Line, contextWriteScalar)
	}
}

// Enabled reports whether c says anything at all. An empty ContextSpec is
// rejected at LoadConfig rather than silently accepted as a no-op, the same
// treatment HandoffSpec.Enabled() drives.
//
// A stated fidelity counts even though it enables no tool: `context:
// {fidelity: off}` is the opt-out, which is the whole point of the field, and
// reading it as "enables nothing" would reject the one spelling a step most
// wants.
func (c *ContextSpec) Enabled() bool {
	return c.Write || c.Fidelity != "" || c.Qualify || len(c.From) > 0
}

// WritesContext reports whether this step is granted the set_context tool.
func (s Step) WritesContext() bool {
	return s.Context != nil && s.Context.Write
}

// QualifiesContext reports whether this step's matrix records per-cell facts:
// each cell scoped, merged at the join under a key naming the cell.
func (s Step) QualifiesContext() bool {
	return s.Context != nil && s.Context.Qualify
}

// ReservedContextPrefix is the key namespace the engine keeps for itself. A
// set_context call naming a key under it is refused at the tool boundary, so a
// model cannot overwrite engine bookkeeping by guessing a key name.
const ReservedContextPrefix = "internal."

// MaxContextKeyLen bounds a context key. Keys come from a model, so a
// pathological one is data, not a bug — but it still ends up in a table, in a
// recap, and in log lines, so it gets a ceiling.
const MaxContextKeyLen = 128

// MaxContextValueLen bounds one recorded value, whoever wrote it. A value is
// quoted into a later model's context, so an unbounded one is a way to spend a
// downstream step's whole window on a single fact. Shared by the set_context
// tool boundary and the task collector so both refuse at the same size.
const MaxContextValueLen = 8192

// ContextDir is the directory in a task's working space whose files a
// `context: write` task records. Reserved as an artifact name — an artifact so
// called would materialize over it (see ValidateArtifactName).
//
// The file NAME is the key, verbatim, extension included: a task that wants
// the key `failure_cause` writes `context/failure_cause`. Stripping an
// extension would be the kind of quiet rule that makes `a.txt` and `a.md`
// collide for reasons nobody can see in the pipeline.
const ContextDir = "context"

// ValidateContextKey reports whether key is one a step may write. Shared by
// the tool boundary (internal/agent) so the rule is stated once.
//
// The charset is deliberately narrow: keys are identifiers a later step reads
// back by name, and allowing whitespace or quoting characters would make a key
// that renders one way in a recap and matches another way on lookup.
func ValidateContextKey(key string) error {
	if key == "" {
		return errors.New("context key must not be empty")
	}

	if len(key) > MaxContextKeyLen {
		return fmt.Errorf("context key is %d characters, above the limit of %d", len(key), MaxContextKeyLen)
	}

	if strings.HasPrefix(key, ReservedContextPrefix) {
		return fmt.Errorf("context key %q uses the reserved %q prefix", key, ReservedContextPrefix)
	}

	// "." and ".." pass the charset below but are path traversal everywhere a
	// key is also a file name (see ContextDir). A directory listing never
	// yields them, so this is belt and braces rather than a live hole — but the
	// charset is the wrong place to learn that from.
	if key == "." || key == ".." {
		return fmt.Errorf("context key %q is a path element, not a name", key)
	}

	for _, r := range key {
		if !isContextKeyRune(r) {
			return fmt.Errorf("context key %q contains %q; use letters, digits, and _ - . only", key, r)
		}
	}

	return nil
}

// isContextKeyRune reports whether r may appear in a context key.
func isContextKeyRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '-', r == '.':
		return true
	default:
		return false
	}
}

// validateContextSteps enforces the context: rules at load time: agent steps
// only, never a hook, never inside a concurrent block, and it must enable
// something.
func (c *Config) validateContextSteps() error {
	err := validateFidelity("defaults context", c.defaultFidelity(), 0)
	if err != nil {
		return err
	}

	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitHookSteps(rejectContextOnHook)
		if err != nil {
			return err
		}

		err = job.visitSteps(checkContextStep)
		if err != nil {
			return err
		}

		err = c.rejectContextInBranches(job)
		if err != nil {
			return err
		}
	}

	return c.validateContextQualify()
}

// rejectContextOnHook rejects context: on a hook step. A hook is a reaction
// that runs outside the plan's ordering, so what it stored — and whether the
// steps that read it had already run — would depend on when it happened to
// fire.
func rejectContextOnHook(label string, step *Step) error {
	if step.Context != nil {
		return fmt.Errorf("%s: context is not valid on hook steps", label)
	}

	return nil
}

// checkContextStep validates one step's context:, if it sets one.
//
// Both halves are legal on an agent step. A TASK may write — it records facts
// by writing files into ContextDir, which the runner collects — but may not
// read: fidelity: renders a recap into a conversation, and a shell command has
// none. That asymmetry is also what keeps caching honest, since a command
// whose text depended on a runtime fact could not be hashed at plan time.
func checkContextStep(label string, step *Step) error {
	if step.Context == nil {
		return nil
	}

	if step.Agent == "" && step.Task == "" {
		return fmt.Errorf("%s: context is only valid on agent and task steps", label)
	}

	if step.Agent == "" && step.Context.Fidelity != "" {
		return fmt.Errorf("%s: context fidelity is only valid on agent steps; a task writes context but never reads it", label)
	}

	if !step.Context.Enabled() {
		return fmt.Errorf("%s: context enables nothing (set write, from, fidelity and/or qualify)", label)
	}

	return nil
}

// validateContextQualify enforces qualify:'s contract across every job.
//
// It walks the plan itself rather than riding visitSteps, because the two
// halves of the question sit on different steps once a try: is involved:
// across: and max_in_flight: are on the WRAPPER (it is the step with a
// position in the plan), while context: is on the agent it wraps. visitSteps
// visits those separately, so either half alone would answer the wrong
// question — the inner step looking like a context write with no matrix, and
// the wrapper like a concurrent matrix that writes nothing.
func (c *Config) validateContextQualify() error {
	for _, job := range c.Jobs {
		for i := range job.Plan {
			err := visitQualifyStep(fmt.Sprintf("job %q step %d", job.Name, i), &job.Plan[i])
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// visitQualifyStep checks one plan step and any concurrent block beneath it.
func visitQualifyStep(label string, step *Step) error {
	inner := unwrapStep(step)

	err := checkContextQualify(label, step, inner)
	if err != nil {
		return err
	}

	for kind, branches := range branchesOf(inner) {
		for b := range branches {
			err = visitQualifyStep(fmt.Sprintf("%s (%s branch %d)", label, kind, b), &branches[b])
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// checkContextQualify enforces the two halves of qualify:'s contract — that it
// describes a matrix that writes, and that a concurrent matrix says so.
//
// The second is the whole reason the field exists. Concurrent cells MUST be
// scoped, so before qualify: it was max_in_flight: that decided the key names a
// downstream step reads: one pipeline recorded `finding` serially and
// `reviewer__dim_api_.finding` at max_in_flight: 4, with nothing to tell the
// author that a step reading a key by name had stopped finding it. Refusing
// here turns that silent rename into a load error naming the one-line fix.
//
// outer carries the matrix (across:, max_in_flight:); inner carries context:.
// They are the same step unless a try: wraps it.
func checkContextQualify(label string, outer, inner *Step) error {
	if inner.QualifiesContext() {
		switch {
		case len(outer.Across) == 0:
			return fmt.Errorf("%s: context qualify: names the cell each fact came from, so it is only valid on an across: step", label)
		case !inner.WritesContext():
			return fmt.Errorf("%s: context qualify: describes how this step's writes are keyed, but the step does not write (set write: true)", label)
		}

		return nil
	}

	if outer.MaxInFlight > 1 && inner.WritesContext() {
		return fmt.Errorf("%s: max_in_flight: %d with context write requires context qualify: true — concurrent cells cannot share one key, so each cell's facts are recorded under a key naming it (finding becomes %q); saying so here is what keeps the key names the same whether or not the cells run concurrently",
			label, outer.MaxInFlight, "reviewer [dim=api].finding")
	}

	return nil
}

// rejectContextInBranches refuses the one shape of concurrent context write
// that cannot be made safe: a TASK writing under the shared workspace.
//
// Concurrent writes to the STORE are fine now — a branch writes to a scope
// only it touches and internal/pipeline merges those back at the join, in
// declaration order, under keys naming the branch. But a task records by
// writing files into ContextDir of its working directory, and under the shared
// strategy every step's working directory is the same build root. Two branches
// both writing `context/finding` are two writers on one file: the value that
// survives is neither of theirs. Observed as `n+1 query credential` — one
// branch's bytes overlaid on the other's.
//
// Under copy/btrfs each branch has its own directory, so there is no collision
// and this allows it. An AGENT is unaffected either way: set_context is a tool
// call, and never touches the filesystem.
func (c *Config) rejectContextInBranches(job Job) error {
	if c.Workspace != nil {
		return nil
	}

	for i := range job.Plan {
		label := fmt.Sprintf("job %q step %d", job.Name, i)
		step := unwrapStep(&job.Plan[i])

		for kind, branches := range branchesOf(step) {
			for b := range branches {
				err := visitStepTree(fmt.Sprintf("%s (%s branch %d)", label, kind, b), &branches[b], rejectSharedBranchTaskWrite)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// rejectSharedBranchTaskWrite rejects one concurrent task's context: write
// under the shared workspace strategy.
func rejectSharedBranchTaskWrite(label string, step *Step) error {
	if step.Task != "" && step.WritesContext() {
		return fmt.Errorf("%s: a task cannot write context inside a concurrent block under the shared workspace — every branch writes into one %s/ directory, so concurrent branches would overwrite each other's files. Set workspace.strategy (copy or btrfs), or record the facts from an agent step instead", label, ContextDir)
	}

	return nil
}

// unmarshalMapping decodes the {write, fidelity, qualify, from} form. Split
// from UnmarshalYAML so neither half carries the other's branching.
func (c *ContextSpec) unmarshalMapping(value *yaml.Node) error {
	err := rejectUnknownKeys(value, "step context", "write", "fidelity", "qualify", "from")
	if err != nil {
		return err
	}

	var m struct {
		Write    bool            `yaml:"write"`
		Fidelity ContextFidelity `yaml:"fidelity"`
		Qualify  bool            `yaml:"qualify"`
		From     yaml.Node       `yaml:"from"`
	}

	err = value.Decode(&m)
	if err != nil {
		return fmt.Errorf("step context: %w", err)
	}

	err = validateFidelity("step context", m.Fidelity, value.Line)
	if err != nil {
		return err
	}

	if !m.From.IsZero() {
		c.From, err = decodeContextFrom(&m.From)
		if err != nil {
			return err
		}
	}

	c.Write, c.Fidelity, c.Qualify = m.Write, m.Fidelity, m.Qualify

	return nil
}
