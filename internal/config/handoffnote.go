package config

import (
	"fmt"
)

// HandoffNoteDir is the build-workspace directory holding rendered handoff
// notes, one per sending step (see HandoffNotePath). Reserved as an artifact
// name — a resource or output called "handoff" would materialize over it in
// the shared build root.
const HandoffNoteDir = "handoff"

// readFileToolName is the builtin the note's delivery depends on: it arrives
// as a synthetic read_file result, exactly as context_paths does.
const readFileToolName = "read_file"

// HandoffNotePath is the workspace-relative path of the note written by the
// agent step named sender. It is the single place the layout is defined:
// internal/agent renders to it and reads back from it, and it is relative
// (not absolute) because it is resolved against the receiving step's own
// working directory, the same way context_paths entries are.
func HandoffNotePath(sender string) string {
	return HandoffNoteDir + "/" + sender + ".md"
}

// validateHandoffNoteSteps enforces the handoff_note: rules at load time and
// resolves each receiving step's HandoffNoteFrom.
//
// A note flows FORWARD, unlike handoff:, which carries context backward along
// a to:/verdicts: route. The receiver is computed rather than declared: the
// next agent step after the sender, within the same get-segment. That keeps
// the configuration surface to a single boolean, and — because the receiver
// re-resolves the path on every dispatch — makes delivery idempotent across a
// to:-driven redo without any carry through internal/pipeline.
func (c *Config) validateHandoffNoteSteps() error {
	for i := range c.Jobs {
		err := c.Jobs[i].visitHookSteps(rejectHandoffNoteOnHook)
		if err != nil {
			return err
		}

		err = c.validateHandoffNoteSegments(&c.Jobs[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// rejectHandoffNoteOnHook rejects handoff_note: on a hook step: a hook is a
// reaction, not a positioned step with a successor to hand off to.
func rejectHandoffNoteOnHook(label string, step *Step) error {
	if step.HandoffNote {
		return fmt.Errorf("%s: handoff_note is not valid on hook steps", label)
	}

	return nil
}

// validateHandoffNoteSegments splits job's plan into get-bounded segments (the
// same split validateHandoffSegments uses) and validates each. A note never
// crosses a get boundary: a get fans the remainder of the plan out per
// version, so each fanned-out build is an independent run of the steps after
// it, and a note from before the fan-out has no single successor.
func (c *Config) validateHandoffNoteSegments(job *Job) error {
	var segment []int

	flush := func() error {
		if len(segment) == 0 {
			return nil
		}

		err := c.validateHandoffNoteSegment(job, segment)
		segment = nil

		return err
	}

	for i := range job.Plan {
		if job.Plan[i].Get != "" {
			err := flush()
			if err != nil {
				return err
			}

			continue
		}

		segment = append(segment, i)
	}

	return flush()
}

// validateHandoffNoteSegment checks every handoff_note: step in segment (plan
// indices in declaration order) and wires each note to its receiver. It walks
// the segment once, carrying the most recent sender forward across
// intervening task/put steps — a build-check between an implementer and a
// reviewer must not break the chain.
func (c *Config) validateHandoffNoteSegment(job *Job, segment []int) error {
	sender := ""

	for _, idx := range segment {
		step := &job.Plan[idx]

		if step.Agent == "" {
			if step.HandoffNote {
				return fmt.Errorf("job %q step %d: handoff_note is only valid on agent steps", job.Name, idx)
			}

			continue
		}

		// Read before write: a step both receiving and sending (the middle of
		// a chain) takes the previous sender, then becomes the sender itself.
		step.HandoffNoteFrom = sender

		if sender != "" {
			err := c.checkHandoffNoteReceiver(job, idx, *step)
			if err != nil {
				return err
			}
		}

		if step.HandoffNote {
			err := c.checkHandoffNoteSender(job, idx)
			if err != nil {
				return err
			}

			sender = stepName(*step)
		}
	}

	return checkHandoffNoteDelivered(job, segment)
}

// checkHandoffNoteSender validates one sending step. Under workspace
// isolation each step gets its own materialized directory and only DECLARED
// outputs survive it, so a note written to the build root would be discarded
// with the sender's space and the receiver would find nothing. Fail loudly at
// load rather than silently dropping it.
func (c *Config) checkHandoffNoteSender(job *Job, idx int) error {
	if c.Workspace != nil {
		return fmt.Errorf("job %q step %d: handoff_note is not supported with workspace strategy %q (the note cannot cross an isolated step boundary)", job.Name, idx, c.Workspace.Strategy)
	}

	return nil
}

// checkHandoffNoteReceiver rejects a receiving step that cannot read its
// note. Delivery reuses the context_paths machinery, which requires read_file
// in the grant (see prepareContextBlocks) — without it the receiving step
// would fail at preparation, and the pipeline author would be debugging the
// innocent step rather than the one that misconfigured the edge.
func (c *Config) checkHandoffNoteReceiver(job *Job, idx int, step Step) error {
	agent, err := c.FindAgent(step.Agent)
	if err != nil {
		return fmt.Errorf("job %q step %d: %w", job.Name, idx, err)
	}

	specs, err := resolveEffectiveTools(agent.Tools, step.Tools)
	if err != nil {
		return fmt.Errorf("job %q step %d: %w", job.Name, idx, err)
	}

	// An empty grant means the builtin default set (see buildAgentTools),
	// which includes read_file.
	if len(specs) == 0 {
		return nil
	}

	for _, spec := range specs {
		if spec.Builtin == readFileToolName {
			return nil
		}
	}

	return fmt.Errorf("job %q step %d: receives a handoff_note from step %q but does not grant %s", job.Name, idx, step.HandoffNoteFrom, readFileToolName)
}

// checkHandoffNoteDelivered rejects a sending step whose note nothing
// receives — dead config, the same treatment handoff: gets — and rejects a
// receiver that cannot read it.
func checkHandoffNoteDelivered(job *Job, segment []int) error {
	senders := map[string]int{}
	received := map[string]bool{}

	for _, idx := range segment {
		step := job.Plan[idx]

		if step.HandoffNoteFrom != "" {
			received[step.HandoffNoteFrom] = true
		}

		if step.HandoffNote {
			senders[stepName(step)] = idx
		}
	}

	for name, idx := range senders {
		if !received[name] {
			return fmt.Errorf("job %q step %d: handoff_note is set but no later agent step in this segment receives it", job.Name, idx)
		}
	}

	return nil
}
