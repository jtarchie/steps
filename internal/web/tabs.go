package web

import (
	"fmt"

	"github.com/jtarchie/steps/internal/runview"
)

// tabStrip is the tabs over a step that ran more than once: a to: loop's passes, or a retried step's attempts. Concourse draws the same for a retry, as numbers whose status is a color alone; here the glyph carries it too, and the label says it in words.
type tabStrip struct {
	Label string
	Tabs  []tabView
}

type tabView struct {
	ID, Controls, Class, Glyph, Label string
	N                                 int
	Selected                          bool
}

// tabsOf is nil for a step with nothing to tab between. The last tab is the one selected, as Concourse selects the newest attempt; a reader's own pick is app.js's to keep.
func (l *locale) tabsOf(step *stepView) *tabStrip {
	if step.Kind == runview.KindLoop {
		passes := step.Children
		strip := &tabStrip{Label: l.text("tabs.passes", step.Name)}

		for k, pass := range passes {
			strip.Tabs = append(strip.Tabs, tabView{
				ID: pass.Anchor() + "_tab", Controls: pass.Anchor(), N: k + 1, Selected: k == len(passes)-1,
				Class: outcomeClass(pass), Glyph: outcomeMark(pass),
				Label: l.text("tab.pass", k+1, len(passes), l.outcome(pass)),
			})
		}

		return strip
	}

	attempts := step.Attempts()
	if len(attempts) < 2 {
		return nil
	}

	strip := &tabStrip{Label: l.text("tabs.attempts", step.Name)}

	for _, attempt := range attempts {
		id := attemptPanel(step, attempt)
		strip.Tabs = append(strip.Tabs, tabView{
			ID: id + "_tab", Controls: id, N: attempt.N, Selected: attempt.Last,
			Class: statusWord(attempt.Status), Glyph: statusMark(attempt.Status),
			Label: l.text("tab.attempt", attempt.N, attempt.Of, l.word("status", statusWord(attempt.Status))),
		})
	}

	return strip
}

func attemptPanel(step *stepView, attempt runview.Attempt) string {
	return fmt.Sprintf("%s_try%d", step.Anchor(), attempt.N)
}

// outcome is how a step ended, in words a screen reader says: a decision the run went on from is named as one.
func (l *locale) outcome(step *stepView) string {
	switch {
	case step.Routed():
		return l.text("status.routedto", step.RoutedTo)
	case step.Tolerated():
		return l.text("status.tolerated")
	default:
		return l.word("status", statusWord(step.Status))
	}
}

func outcomeClass(step *stepView) string {
	switch {
	case step.Routed():
		return "routed"
	case step.Tolerated():
		return "tolerated"
	default:
		return statusWord(step.Status)
	}
}

func outcomeMark(step *stepView) string {
	switch {
	case step.Routed():
		return "↪"
	case step.Tolerated():
		return "⊘"
	default:
		return statusMark(step.Status)
	}
}
