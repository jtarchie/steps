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

// visitStepTree calls fn for a step and everything nested inside it: what a
// try: wraps, a concurrent block's branches, a do: block's children, and every
// hook — each labelled with where it sits.
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
