package config

// The race: step — run several steps at once, keep whichever finishes first.

import (
	"fmt"
	"slices"
)

// Race runs its branches concurrently and keeps whichever completes
// successfully first, cancelling the rest.
//
// ⚠️ This is a LATENCY hedge, not a cost saver, and that is the single most
// important thing to know about it. Running both branches always costs both,
// every time, even when the fast one wins. The value is "never wait for the
// slow path", not "spend less" — anyone reaching for race: to save money wants
// caching or plain retries instead.
type Race struct {
	// Steps are the racers. Fewer than two is a load error: a race with one
	// runner is a step with extra words, and a race with none is a typo.
	Steps []Step `yaml:"steps"`
}

// validateRace enforces the shape of a race: block and the two structural
// requirements that make its result usable.
func (c *Config) validateRace() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Race == nil {
				return nil
			}

			return c.validateRaceBlock(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateRaceBlock(label string, step *Step) error {
	if len(step.Race.Steps) < 2 {
		return fmt.Errorf("%s: race.steps needs at least two branches; a race with one runner is just a step", label)
	}

	// Workspace isolation, required. Losing branches are cancelled while they
	// may be mid-write, and under the shared single-directory workspace that
	// means a cancelled loser can corrupt the winner's files. There is no
	// version of this that is safe without isolation.
	if c.Workspace == nil {
		return fmt.Errorf("%s: race: requires workspace isolation (set a top-level workspace: strategy); losing branches are cancelled mid-run and would otherwise share one mutable directory with the winner", label)
	}

	// Identical outputs, required. Downstream steps must not have to know
	// which branch won — the winner's outputs ARE the step's outputs, so the
	// branches have to agree on what those are.
	want := slices.Clone(step.Race.Steps[0].Outputs)
	slices.Sort(want)

	for i := 1; i < len(step.Race.Steps); i++ {
		got := slices.Clone(step.Race.Steps[i].Outputs)
		slices.Sort(got)

		if !slices.Equal(want, got) {
			return fmt.Errorf("%s: every race branch must declare the same outputs (branch 0 has %v, branch %d has %v); a downstream step cannot depend on which branch won",
				label, want, i, got)
		}
	}

	return c.rejectOperationFields(label, step, "a race")
}

// rejectOperationFields rejects the fields that describe an operation, on a
// block that performs none. Shared by race: and in_parallel:, which are both
// containers: they fetch nothing, run nothing, and produce nothing of their
// own, so every field describing an operation belongs on a branch.
//
// kind carries its own article ("an in_parallel", "a race") so the message
// reads as English rather than as a template.
func (c *Config) rejectOperationFields(label string, step *Step, kind string) error {
	for _, rejected := range []struct {
		name string
		set  bool
	}{
		{"inputs", step.InputsDeclared()},
		{"outputs", step.Outputs != nil},
		{"image", step.Image != ""},
		{"run", step.Run != ""},
		{"prompt", step.Prompt != ""},
		{"params", step.Params != nil},
		{"trigger", step.Trigger},
		{"version", step.Version != nil},
		{"verdicts", len(step.Verdicts) > 0},
		{"assert", step.Assert != nil},
	} {
		if rejected.set {
			return fmt.Errorf("%s: %s is not valid on %s step; set it on the step inside the block that it describes",
				label, rejected.name, kind)
		}
	}

	return nil
}
