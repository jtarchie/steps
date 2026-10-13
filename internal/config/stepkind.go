package config

// What kind of step a Step is, and how to walk one.

import (
	"fmt"
	"strings"
)

// ReferencesEntities reports whether a step of this kind names something the
// pipeline has to resolve — a resource, a task, an agent, or branches that do.
func (k StepKind) ReferencesEntities() bool {
	return k != StepKindLoadVar && k != StepKindApproval
}

// StepKind is which of Get/Task/Put/Agent a Step is. See Step.Kind.
type StepKind string

// The StepKind values, one per Step field Kind can resolve to. Each is also
// the YAML key that selects it.
const (
	StepKindGet   StepKind = "get"
	StepKindTask  StepKind = "task"
	StepKindPut   StepKind = "put"
	StepKindAgent StepKind = "agent"
	StepKindTry   StepKind = "try"
	// StepKindInParallel is a block of steps that run concurrently. Adding it
	// to stepKinds is what makes `go run ./tools/kindswitch ./...` demand an
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

// nesting is how a kind holds the steps nested under it.
type nesting int

const (
	leaf nesting = iota
	// wraps is try:, one step it runs and tolerates the failure of.
	wraps
	// sequence is do:, steps that run one after another as one step.
	sequence
	// branches is a concurrent block: in_parallel:, race:, ensemble:.
	branches
)

type stepKindSpec struct {
	kind  StepKind
	isSet func(s *Step) bool
	nest  nesting
	// children points at the nested steps in place, so a walker may rewrite
	// them. Nil for a leaf.
	children func(s *Step) []*Step
	// branchesKey is the key under a branches kind's mapping that holds its
	// list.
	branchesKey string
}

// stepKinds is the one description of every step kind: what selects it, what
// it nests and how that nesting is spelled in YAML. Kind, both kind errors,
// the tree walkers and the in_parallel: shorthand rewrite all read it, so a
// kind cannot be known to one and missing from another — which is how a do: +
// task: step came to be reported as setting only task:. tools/kindswitch reads
// the Step fields these rows test as the kind set, in this order.
//
//nolint:gochecknoglobals // a read-only table
var stepKinds = [...]stepKindSpec{
	{kind: StepKindGet, isSet: func(s *Step) bool { return s.Get != "" }},
	{kind: StepKindTask, isSet: func(s *Step) bool { return s.Task != "" }},
	{kind: StepKindPut, isSet: func(s *Step) bool { return s.Put != "" }},
	{kind: StepKindAgent, isSet: func(s *Step) bool { return s.Agent != "" }},
	{
		kind: StepKindTry, isSet: func(s *Step) bool { return s.Try != nil }, nest: wraps,
		children: func(s *Step) []*Step {
			if s.Try == nil {
				return nil
			}

			return []*Step{s.Try}
		},
	},
	{
		kind: StepKindInParallel, isSet: func(s *Step) bool { return s.InParallel != nil },
		nest: branches, branchesKey: "steps",
		children: func(s *Step) []*Step {
			if s.InParallel == nil {
				return nil
			}

			return stepPointers(s.InParallel.Steps)
		},
	},
	{
		kind: StepKindRace, isSet: func(s *Step) bool { return s.Race != nil },
		nest: branches, branchesKey: "steps",
		children: func(s *Step) []*Step {
			if s.Race == nil {
				return nil
			}

			return stepPointers(s.Race.Steps)
		},
	},
	{
		kind: StepKindEnsemble, isSet: func(s *Step) bool { return s.Ensemble != nil },
		nest: branches, branchesKey: "agents",
		children: func(s *Step) []*Step {
			if s.Ensemble == nil {
				return nil
			}

			return stepPointers(s.Ensemble.Agents)
		},
	},
	{kind: StepKindLoadVar, isSet: func(s *Step) bool { return s.LoadVar != "" }},
	{kind: StepKindApproval, isSet: func(s *Step) bool { return s.Approval != nil }},
	{
		kind: StepKindDo, isSet: func(s *Step) bool { return s.Do != nil }, nest: sequence,
		children: func(s *Step) []*Step { return stepPointers(s.Do) },
	},
}

func stepPointers(steps []Step) []*Step {
	out := make([]*Step, len(steps))
	for i := range steps {
		out[i] = &steps[i]
	}

	return out
}

func stepKindByKey(key string) (stepKindSpec, bool) {
	for _, spec := range stepKinds {
		if string(spec.kind) == key {
			return spec, true
		}
	}

	return stepKindSpec{}, false
}

// childLabel is how a validation error names the i'th step nested under spec.
func (spec stepKindSpec) childLabel(parent string, i int) string {
	switch spec.nest {
	case wraps:
		return fmt.Sprintf("%s (%s)", parent, spec.kind)
	case sequence:
		return fmt.Sprintf("%s (%s step %d)", parent, spec.kind, i)
	case branches:
		return fmt.Sprintf("%s (%s branch %d)", parent, spec.kind, i)
	case leaf:
	}

	return parent
}

// Kind reports which single kind of step s is. ok is false when zero, or
// more than one, of the kinds is set — a malformed step every call site
// should reject the same way, rather than each silently picking whichever
// field its own historical check order happened to test first.
func (s Step) Kind() (kind StepKind, ok bool) {
	for _, spec := range stepKinds {
		if !spec.isSet(&s) {
			continue
		}

		if ok {
			return "", false // a second kind field was set — reject, don't silently keep the first
		}

		kind, ok = spec.kind, true
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
				return fmt.Errorf("%s: step names no kind, set one of %s", label, allStepKinds())
			}

			return fmt.Errorf("%s: step sets %s, but a step is exactly one of %s",
				label, strings.Join(set, " and "), allStepKinds())
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func allStepKinds() string {
	keys := make([]string, 0, len(stepKinds))
	for _, spec := range stepKinds {
		keys = append(keys, string(spec.kind))
	}

	return strings.Join(keys, "/")
}

// kindFieldsSet names the kinds this step sets, in table order, for the
// "sets task and agent" half of validateStepKinds' message.
func (s Step) kindFieldsSet() []string {
	var set []string

	for _, spec := range stepKinds {
		if spec.isSet(&s) {
			set = append(set, string(spec.kind))
		}
	}

	return set
}

// branchesOf returns a concurrent block's kind and branches, or nothing for
// any other step. It exists so the walkers below treat in_parallel:, race:
// and ensemble: identically — they differ in what their branches' outcomes
// MEAN, never in the fact that they have branches, and a walker that knew
// about one but not the other is precisely the silent gap this codebase has
// shipped before.
//
// do: is deliberately not a branches kind: this function's callers (context
// scoping, handoff-note fan-in/broadcast, var flow) attach concurrency
// semantics to whatever it returns, and a do: block is sequential, so its
// children behave like ordinary consecutive plan steps.
func branchesOf(step *Step) (StepKind, []*Step) {
	for _, spec := range stepKinds {
		if spec.nest == branches && spec.isSet(step) {
			return spec.kind, spec.children(step)
		}
	}

	return "", nil
}

// visitStepTree calls fn for a step and everything nested inside it: what a
// try: wraps, a concurrent block's branches, a do: block's children, and every
// hook — each labelled with where it sits.
func visitStepTree(label string, step *Step, fn func(label string, step *Step) error) error {
	return walkStepTree(label, step, struct{}{}, carryNothing, dropCarried(fn))
}

func carryNothing(string, *Step, struct{}) (struct{}, struct{}) { return struct{}{}, struct{}{} }

func dropCarried(fn func(label string, step *Step) error) func(string, *Step, struct{}) error {
	return func(label string, step *Step, _ struct{}) error { return fn(label, step) }
}

// walkStepTree is visitStepTree carrying a value down the tree — the one
// recursion both share, so a container kind cannot be walked by one and missed
// by the other. fn sees what the step received; pass decides what the step's
// CHILDREN (a try: body, branches, do: steps) receive and what its HOOKS
// receive, and is asked before fn so fn may rewrite the step.
func walkStepTree[T any](label string, step *Step, carried T, pass func(string, *Step, T) (T, T), fn func(string, *Step, T) error) error {
	inner, hooks := pass(label, step, carried)

	err := fn(label, step, carried)
	if err != nil {
		return err
	}

	// Without this every validator in this package would silently stop at a
	// container, and what it holds could carry anything at all.
	for _, spec := range stepKinds {
		if spec.children == nil {
			continue
		}

		for i, child := range spec.children(step) {
			err = walkStepTree(spec.childLabel(label, i), child, inner, pass, fn)
			if err != nil {
				return err
			}
		}
	}

	return step.Hooks.Each(func(name string, hook *Step) error {
		return walkStepTree(fmt.Sprintf("%s (%s hook)", label, name), hook, hooks, pass, fn)
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
