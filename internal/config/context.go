package config

// A step's context: — the run-scoped key/value store an agent step may write
// with the synthesized set_context tool.

import (
	"errors"
	"fmt"
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
// enable another.
type ContextSpec struct {
	Write bool
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
		err := rejectUnknownKeys(value, "step context", "write")
		if err != nil {
			return err
		}

		var m struct {
			Write bool `yaml:"write"`
		}

		err = value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step context: %w", err)
		}

		c.Write = m.Write

		return nil
	default:
		return fmt.Errorf("step context at line %d must be %q or a {write} mapping", value.Line, contextWriteScalar)
	}
}

// Enabled reports whether c turns on anything. An all-false ContextSpec is
// rejected at LoadConfig rather than silently accepted as a no-op, the same
// treatment HandoffSpec.Enabled() drives.
func (c *ContextSpec) Enabled() bool {
	return c.Write
}

// WritesContext reports whether this step is granted the set_context tool.
func (s Step) WritesContext() bool {
	return s.Context != nil && s.Context.Write
}

// ReservedContextPrefix is the key namespace the engine keeps for itself. A
// set_context call naming a key under it is refused at the tool boundary, so a
// model cannot overwrite engine bookkeeping by guessing a key name.
const ReservedContextPrefix = "internal."

// MaxContextKeyLen bounds a context key. Keys come from a model, so a
// pathological one is data, not a bug — but it still ends up in a table, in a
// recap, and in log lines, so it gets a ceiling.
const MaxContextKeyLen = 128

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

		err = rejectContextInBranches(job)
		if err != nil {
			return err
		}
	}

	return nil
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
func checkContextStep(label string, step *Step) error {
	if step.Context == nil {
		return nil
	}

	if step.Agent == "" {
		return fmt.Errorf("%s: context is only valid on agent steps", label)
	}

	if !step.Context.Enabled() {
		return fmt.Errorf("%s: context enables nothing (set write)", label)
	}

	return nil
}

// rejectContextInBranches rejects context: on a step inside a concurrent block
// or on an across: step.
//
// Concurrent branches each work from their own copy of the context, so a
// branch's writes have to surface at the join rather than merge into the
// shared store — two branches writing one key into one store resolve to
// whichever finished last, the same hazard validateParallelOutputs already
// refuses for artifact names. The join half does not exist yet, so a write in
// a branch would be silently discarded. A load error until it does.
func rejectContextInBranches(job Job) error {
	for i := range job.Plan {
		label := fmt.Sprintf("job %q step %d", job.Name, i)
		step := unwrapStep(&job.Plan[i])

		if len(job.Plan[i].Across) > 0 && step.Context != nil {
			return fmt.Errorf("%s: context is not supported on an across: step yet — each cell writes its own copy, and the join that collects them does not exist", label)
		}

		for kind, branches := range branchesOf(step) {
			for b := range branches {
				err := visitStepTree(fmt.Sprintf("%s (%s branch %d)", label, kind, b), &branches[b], rejectNestedContext)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// rejectNestedContext rejects context: on one step inside a concurrent block.
func rejectNestedContext(label string, step *Step) error {
	if step.Context != nil {
		return fmt.Errorf("%s: context is not supported inside a concurrent block yet — a branch writes its own copy, and the join that collects it does not exist", label)
	}

	return nil
}
