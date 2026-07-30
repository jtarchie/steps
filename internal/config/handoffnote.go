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
			err := c.checkHandoffNoteSender(job, idx, *step)
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
//
// A resource named HandoffNoteDir is rejected for the same reason: under the
// shared strategy a get materializes into <build>/<name>, which would be the
// very directory the note is written into — and resource names, unlike step
// inputs:/outputs:, never pass through ValidateArtifactName on that path.
func (c *Config) checkHandoffNoteSender(job *Job, idx int, step Step) error {
	if c.Workspace != nil {
		return fmt.Errorf("job %q step %d: handoff_note is not supported with workspace strategy %q (the note cannot cross an isolated step boundary)", job.Name, idx, c.Workspace.Strategy)
	}

	err := checkHandoffNoteStepDir(job, idx, step, "sends")
	if err != nil {
		return err
	}

	for _, resource := range c.Resources {
		if resource.Name == HandoffNoteDir {
			return fmt.Errorf("job %q step %d: handoff_note is set but a resource is named %q, which is reserved for handoff notes", job.Name, idx, HandoffNoteDir)
		}
	}

	return nil
}

// checkHandoffNoteStepDir rejects dir: on either end of a note edge. The note
// is written under the SENDER's working directory and read back relative to
// the RECEIVER's, so a dir: on either side breaks that equivalence: the note
// would be written inside a materialized input artifact (dirtying it), and the
// receiver — whose paths are confined by resolveAgentPath, which rejects ".."
// — could never reach the build root to read it. Today that miss is silent
// (an absent note is skipped by design), which makes it a load error's job.
func checkHandoffNoteStepDir(job *Job, idx int, step Step, role string) error {
	if step.Dir != "" {
		return fmt.Errorf("job %q step %d: a step that %s a handoff_note cannot set dir: (the note lives in the build root, which dir: %q puts out of reach)", job.Name, idx, role, step.Dir)
	}

	return nil
}

// checkHandoffNoteReceiver rejects a receiving step that cannot read its
// note. Delivery reuses the context_paths machinery, which requires read_file
// in the grant (see prepareContextBlocks) — without it the receiving step
// would fail at preparation, and the pipeline author would be debugging the
// innocent step rather than the one that misconfigured the edge.
func (c *Config) checkHandoffNoteReceiver(job *Job, idx int, step Step) error {
	err := checkHandoffNoteStepDir(job, idx, step, "receives")
	if err != nil {
		return err
	}

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
// receives — dead config, the same treatment handoff: gets.
//
// Both the delivery bookkeeping here and the on-disk note itself are keyed by
// step NAME (see HandoffNotePath), so two senders sharing a name would
// collapse into one entry: the check would pass on the first sender's
// receiver while the second's note went to nobody, and both would write the
// same file. Duplicate sender names are therefore rejected outright rather
// than tracked more cleverly — the name is the address.
func checkHandoffNoteDelivered(job *Job, segment []int) error {
	senders := map[string]int{}
	received := map[string]bool{}

	for _, idx := range segment {
		step := job.Plan[idx]

		if step.HandoffNoteFrom != "" {
			received[step.HandoffNoteFrom] = true
		}

		if !step.HandoffNote {
			continue
		}

		name := stepName(step)
		if prev, dup := senders[name]; dup {
			return fmt.Errorf("job %q step %d: handoff_note is set on two steps named %q in this segment (step %d is the other); a note is addressed by step name, so the names must be unique", job.Name, idx, name, prev)
		}

		senders[name] = idx
	}

	// Reported in plan order, not map order, so a pipeline with two broken
	// edges names the same one every load.
	for _, idx := range segment {
		step := job.Plan[idx]
		if step.HandoffNote && !received[stepName(step)] {
			return fmt.Errorf("job %q step %d: handoff_note is set but no later agent step in this segment receives it", job.Name, idx)
		}
	}

	return nil
}
