package runview

import (
	"fmt"

	"github.com/jtarchie/steps/internal/events"
)

// KindLoop and KindPass are the rows GroupLoops makes: a to: loop, and one pass through it. Neither is a step any event names.
const (
	KindLoop = "loop"
	KindPass = "pass"
)

// Attempt is one try of a retried step; its Outputs keep their place in the step's, which a kept output's link is keyed by.
type Attempt struct {
	N, Of   int
	Status  string
	Outputs []NumberedOutput
	Notes   []Note
	Last    bool
}

// NumberedOutput is an output with its place in the step's Outputs, which is what a kept output's link is keyed by.
type NumberedOutput struct {
	I    int
	Text string
}

// attemptMark is where an attempt after the first began: how many outputs and notes the step held when its note arrived.
type attemptMark struct {
	outputs, notes, n, of int
}

// Tolerated reports a failure a try: ate: the step failed, and the plan carried on as the author asked.
func (s Step) Tolerated() bool { return s.tolerated && s.Failed() }

// Attempts splits a retried step's record at each attempt note, nil for a step that ran once. Every attempt but the last failed, or there would have been no next one.
func (s Step) Attempts() []Attempt {
	if len(s.attempts) == 0 {
		return nil
	}

	bounds := append([]attemptMark{{n: 1, of: s.attempts[0].of}}, s.attempts...)
	out := make([]Attempt, len(bounds))

	for i, mark := range bounds {
		endOutputs, endNotes := len(s.Outputs), len(s.Notes)
		if i+1 < len(bounds) {
			endOutputs, endNotes = bounds[i+1].outputs, bounds[i+1].notes
		}

		attempt := Attempt{N: mark.n, Of: mark.of, Status: events.StatusFailed, Notes: s.Notes[mark.notes:endNotes], Last: i == len(bounds)-1}
		if attempt.Last {
			attempt.Status = s.Status
		}

		for j := mark.outputs; j < endOutputs; j++ {
			attempt.Outputs = append(attempt.Outputs, NumberedOutput{I: j, Text: s.Outputs[j]})
		}

		out[i] = attempt
	}

	return out
}

// markAttempt records where an attempt began; the attempt note itself is not kept, because the tab that shows the attempt says it.
func markAttempt(step *Step, detail string) {
	var n, of int

	_, err := fmt.Sscanf(detail, "%d/%d", &n, &of)
	if err != nil {
		return
	}

	step.attempts = append(step.attempts, attemptMark{outputs: len(step.Outputs), notes: len(step.Notes), n: n, of: of})
}

// GroupLoops gathers each to: loop into one row holding a row per pass, so the page can tab between passes the way Concourse tabs a retried step. The page's and its stream's alone: the terminal draws the plan flat.
func GroupLoops(r *Transcript) {
	r.Roots = groupLoops(r.Roots)
}

func groupLoops(steps []*Step) []*Step {
	for _, step := range steps {
		step.Children = groupLoops(step.Children)
	}

	out := make([]*Step, 0, len(steps))

	for i := 0; i < len(steps); i++ {
		router := steps[i]

		start := loopStart(out, router)
		if start < 0 {
			out = append(out, router)

			continue
		}

		passes := [][]*Step{append(append([]*Step(nil), out[start:]...), router)}
		next := i + 1

		for last := router; last.RoutedTo == router.RoutedTo && sameStep(last, router) && next < len(steps); {
			pass := []*Step{steps[next]}
			next++

			for !sameStep(pass[len(pass)-1], router) && next < len(steps) {
				pass = append(pass, steps[next])
				next++
			}

			passes = append(passes, pass)
			last = pass[len(pass)-1]
		}

		out = append(out[:start], newLoop(router, passes))
		i = next - 1
	}

	return out
}

// loopStart is -1 for a step that did not route, or routed forward. ponytail: one loop at a time; a second route whose span overlaps a loop already drawn is left flat with its chip. Upgrade: merge overlapping spans into one block.
func loopStart(out []*Step, router *Step) int {
	if router.RoutedTo == "" {
		return -1
	}

	for k := len(out) - 1; k >= 0; k-- {
		if out[k].synthetic == "" && out[k].Name == router.RoutedTo {
			return k
		}
	}

	return -1
}

func sameStep(a, b *Step) bool { return a.Index == b.Index && a.Name == b.Name && a.Kind == b.Kind }

// newLoop builds the loop row and its passes. A pass ends as the step that closes it did; one still under way, which has not reached the router again, is running.
func newLoop(router *Step, passes [][]*Step) *Step {
	first := passes[0][0]
	loop := &Step{
		Kind: KindLoop, Name: first.Name + " … " + router.Name, Index: first.Index, ParentID: router.ParentID,
		synthetic: fmt.Sprintf("loop-%d", router.ID), FirstSeq: router.routedSeq, Started: first.Started,
	}

	for k, pass := range passes {
		end := pass[len(pass)-1]
		row := &Step{
			Kind: KindPass, Name: fmt.Sprintf("pass %d", k+1), Index: k + 1, Children: pass,
			synthetic: fmt.Sprintf("pass-%d", pass[0].ID), FirstSeq: pass[0].FirstSeq, Started: pass[0].Started,
		}

		// The first pass is new to a reader the moment the route makes it one: its rows were theirs, loose, and are retracted into it.
		if k == 0 {
			row.FirstSeq = router.routedSeq
		}

		row.Status, row.RoutedTo, row.unreported, row.Ended = end.Status, end.RoutedTo, end.unreported, end.Ended
		if !sameStep(end, router) && !end.Failed() {
			row.Status, row.RoutedTo = "", ""
		}

		for _, step := range pass {
			row.Duration += step.Duration
		}

		loop.Children = append(loop.Children, row)
		loop.Duration += row.Duration
	}

	last := loop.Children[len(loop.Children)-1]
	loop.Status, loop.RoutedTo, loop.unreported, loop.Ended = last.Status, last.RoutedTo, last.unreported, last.Ended

	return loop
}
