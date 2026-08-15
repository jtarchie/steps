package config

// context: { from: { <step>: <level> } } — a step declaring which earlier
// steps' decisions it wants to see.
//
// Declared on the RECEIVER, and nothing arrives without it: a step's inputs
// should be readable off the step, and a sender must never be able to push
// context into a reader that never asked.
//
// The demand is also what creates the obligation. A verdict is always
// recorded, so asking for one costs the sender nothing — but asking for a NOTE
// makes the sender's note required, because a note nobody demanded is one the
// model may reasonably skip, and "chose not to write" is indistinguishable
// afterwards from "forgot".

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// UpstreamDir is the directory a TASK reader's demanded outcomes are
// materialized into, one file per sender named for the step. A shell command
// cannot be handed a synthetic tool result the way an agent can, so the
// filesystem is the interface it gets instead.
//
// Reserved as an artifact name (see ValidateArtifactName): an artifact so
// called would materialize over the delivered outcomes.
const UpstreamDir = "upstream"

// UpstreamPath is the workspace-relative file one sender's outcome is
// delivered at. Relative, because it is resolved against the reading step's
// own working directory.
func UpstreamPath(sender string) string {
	return UpstreamDir + "/" + sender
}

// FromLevel is how much of a named step's outcome a reader is shown.
type FromLevel string

const (
	// FromVerdict delivers the verdict name alone. Demands nothing extra of
	// the sender: a step declaring verdicts: already must emit one.
	FromVerdict FromLevel = "verdict"
	// FromNote delivers the verdict and the sender's note, and REQUIRES that
	// note — the verdict tool's note argument stops being optional.
	FromNote FromLevel = "note"
	// FromFull delivers the verdict, the note, and the sender's final response
	// text. Also requires the note.
	FromFull FromLevel = "full"
)

// fromLevels is the vocabulary in ascending detail, so an error message reads
// as a ladder rather than a set.
var fromLevels = []FromLevel{FromVerdict, FromNote, FromFull} //nolint:gochecknoglobals // static, read-only vocabulary

// Valid reports whether l is one of the known levels.
func (l FromLevel) Valid() bool {
	return slices.Contains(fromLevels, l)
}

// RequiresNote reports whether demanding this level forces the sender to write
// a note.
func (l FromLevel) RequiresNote() bool {
	return l == FromNote || l == FromFull
}

// validateFromLevel checks a stated level, naming the vocabulary (and a
// near-miss, when there is one).
func validateFromLevel(step string, level FromLevel, line int) error {
	if level.Valid() {
		return nil
	}

	known := make([]string, 0, len(fromLevels))
	for _, l := range fromLevels {
		known = append(known, string(l))
	}

	return fmt.Errorf("context from %q at line %d: unknown level %q%s (known: %s)",
		step, line, level, suggestion(string(level), known), strings.Join(known, ", "))
}

// decodeContextFrom decodes the from: mapping, rejecting an empty one — a
// reader that declares from: and names nobody is a half-written line, and
// reading it as "nothing" is how it would stay that way.
func decodeContextFrom(node *yaml.Node) (map[string]FromLevel, error) {
	var raw map[string]FromLevel

	err := node.Decode(&raw)
	if err != nil {
		return nil, fmt.Errorf("step context from: %w", err)
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("step context from at line %d names no steps", node.Line)
	}

	for name, level := range raw {
		err = validateFromLevel(name, level, node.Line)
		if err != nil {
			return nil, err
		}
	}

	return raw, nil
}

// ReadsFrom reports whether this step demands any upstream outcome.
func (s Step) ReadsFrom() bool {
	return len(s.ContextFrom()) > 0
}

// ContextFrom returns the steps this one reads from, keyed by step name. Nil
// when it reads from none.
func (s Step) ContextFrom() map[string]FromLevel {
	inner := s.Unwrap()
	if inner.Context == nil {
		return nil
	}

	return inner.Context.From
}

// FromSenders returns the demanded step names in sorted order — the order the
// delivered blocks are rendered in, and the order the hash sees them. Sorted
// rather than declaration order because a YAML mapping has none, and a reader
// naming two senders must not hash differently run to run.
func (s Step) FromSenders() []string {
	from := s.ContextFrom()
	names := make([]string, 0, len(from))

	for name := range from {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// validateContextFrom checks every from: declaration in the pipeline and
// stamps the resulting obligation onto the steps that must satisfy it.
//
// Two passes per job, because the obligation runs backwards: the reader names
// the sender, so no sender knows it owes a note until every reader has been
// read.
func (c *Config) validateContextFrom() error {
	for i := range c.Jobs {
		job := &c.Jobs[i]

		demands, err := collectFromDemands(job)
		if err != nil {
			return err
		}

		err = stampNoteObligations(job, demands)
		if err != nil {
			return err
		}
	}

	return nil
}

// collectFromDemands validates each reader's from: and returns the highest
// level demanded of each named sender.
func collectFromDemands(job *Job) (map[string]FromLevel, error) {
	demands := map[string]FromLevel{}

	for i := range job.Plan {
		label := fmt.Sprintf("job %q step %d", job.Name, i)
		step := unwrapStep(&job.Plan[i])

		if step.Context == nil || len(step.Context.From) == 0 {
			continue
		}

		if step.Agent == "" && step.Task == "" {
			return nil, fmt.Errorf("%s: context from: is only valid on agent and task steps", label)
		}

		for _, sender := range job.Plan[i].FromSenders() {
			level := step.Context.From[sender]

			if sender == stepName(job.Plan[i]) {
				return nil, fmt.Errorf("%s: context from: names %q, which is this step — a step cannot read its own outcome", label, sender)
			}

			if level.RequiresNote() || demands[sender] == "" {
				demands[sender] = level
			}
		}
	}

	return demands, nil
}

// stampNoteObligations resolves each demanded sender within its job, checks it
// can actually supply what was asked, and marks the ones that owe a note.
func stampNoteObligations(job *Job, demands map[string]FromLevel) error {
	for _, sender := range slices.Sorted(maps.Keys(demands)) {
		step := findPlanStep(job, sender)
		if step == nil {
			return fmt.Errorf("job %q: context from: names %q, which is not a step in this job%s",
				job.Name, sender, suggestion(sender, planStepNames(job)))
		}

		if len(step.VerdictRoutes()) == 0 {
			return fmt.Errorf("job %q: context from: names %q, which declares no verdicts: — it has no decision to hand on", job.Name, sender)
		}

		if demands[sender].RequiresNote() {
			// The demand IS the obligation: internal/agent reads this to make
			// the verdict tool's note argument required.
			unwrapStep(step).NoteRequired = true
		}
	}

	return nil
}

// findPlanStep returns the plan step named name, looking through try:
// wrappers, or nil.
//
// Position is deliberately not checked. A reader may name a step that comes
// LATER in declaration order: that is the revise loop, where the writer reads
// the critic that routes back to it, and the critic sits after the writer in
// the plan.
func findPlanStep(job *Job, name string) *Step {
	for i := range job.Plan {
		if stepName(job.Plan[i]) == name {
			return &job.Plan[i]
		}
	}

	return nil
}

// planStepNames lists the named plan steps, for a did-you-mean.
func planStepNames(job *Job) []string {
	names := make([]string, 0, len(job.Plan))

	for i := range job.Plan {
		if name := stepName(job.Plan[i]); name != "" {
			names = append(names, name)
		}
	}

	return names
}
