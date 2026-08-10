package config

// do: — several steps run one after another, as a single plan step.

import "fmt"

// validateDo enforces the shape of a do: block.
//
// The block is a container, like in_parallel:/race:/ensemble:: it runs nothing
// of its own, so an operation field written on it (inputs:, run:, image:) is a
// step's field on a thing that is not a step, and belongs on one of the steps
// inside. Rejected rather than ignored, since a run: on the block reads as if
// it would run.
func (c *Config) validateDo() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Do == nil {
				return nil
			}

			return c.validateDoBlock(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateDoBlock(label string, step *Step) error {
	if len(step.Do) == 0 {
		return fmt.Errorf("%s: do must list at least one step", label)
	}

	// "a do" reads correctly in rejectOperationFields' message: "... is not
	// valid on a do step; set it on the step inside the block that it
	// describes". handoff: is in that list, which is why validateDoChildPosition
	// below only has to speak about the CHILDREN.
	err := c.rejectOperationFields(label, step, "a do")
	if err != nil {
		return err
	}

	// A get inside a do: would fan the REMAINDER OF THE PLAN out per version
	// from a position that is not a plan position — the same reason try: and
	// the concurrent blocks reject one. The fan-out has nowhere to go.
	//
	// Every other kind is legal inside, including a nested do:, which is
	// pointless but harmless and not worth a rule.
	for i := range step.Do {
		child := &step.Do[i]
		if child.Get != "" {
			return fmt.Errorf("%s: a get step is not valid inside do (a get fans the rest of the plan out per version, which has no meaning inside a block)", label)
		}

		err = validateDoChildPosition(label, i, child)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateDoChildPosition rejects the fields that describe a step's POSITION IN
// A SEGMENT on a step that has no such position.
//
// to:/max_visits: route between plan steps, and handoff:'s backward half
// (context:/tool:) describes arriving via one of those routes. A step inside a
// do: block is not a routing target — the block is the plan-positioned step —
// so all of these would load cleanly and then never fire.
//
// That exact defect is one this codebase has already paid for once: to:/
// max_visits: on the step a try: wraps "used to load fine and silently never
// fire" (see Step.Try). Put them on the do: block itself, which is a real
// position and a legitimate routing target.
func validateDoChildPosition(label string, i int, child *Step) error {
	switch {
	case child.To != nil:
		return fmt.Errorf("%s: to: is not valid on do step %d (a step inside a do: block is not a routing target — put to: on the do: block itself)", label, i)
	case child.MaxVisits != 0:
		return fmt.Errorf("%s: max_visits: is not valid on do step %d (a step inside a do: block is not a routing target — put max_visits: on the do: block itself)", label, i)
	case child.Handoff != nil && child.Handoff.Receives():
		return fmt.Errorf("%s: handoff: context/tool is not valid on do step %d (both describe arriving via a to: route, and a step inside a do: block is never routed to)", label, i)
	case child.WantsNote():
		return fmt.Errorf("%s: handoff: note is not valid on do step %d (a note is addressed to the next agent step in the same segment, and a do: block's children are not segment positions)", label, i)
	default:
		return nil
	}
}
