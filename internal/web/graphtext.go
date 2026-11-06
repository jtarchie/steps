package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// nodeTitle is the detail a node's two lines leave out — what the list view's
// duration and held columns said before the graph was the only view.
func nodeTitle(view jobView) string {
	var parts []string

	if view.HasRun {
		verb := "took"
		if !finished(view.Latest.Status) {
			verb = "running for"
		}

		parts = append(parts, "latest run "+verb+" "+formatDuration(view.Latest.Duration()))
	}

	if view.Held {
		plural := "s"
		if view.Failures == 1 {
			plural = ""
		}

		held := fmt.Sprintf("held after %d failure%s", view.Failures, plural)
		if !view.HeldAt.IsZero() {
			held += ", " + formatAgo(view.HeldAt)
		}

		parts = append(parts, held)
	}

	if view.Queued != nil {
		waiting := "starting"
		if view.Queued.WaitingOn != "" {
			waiting = "waiting: " + view.Queued.WaitingOn
		}

		parts = append(parts, "a run is queued, "+waiting)
	}

	return strings.Join(parts, "; ")
}

// jobLabel is a job node's accessible name: "draft, passed 15 hours ago,
// triggered by weekday, writes draft".
func jobLabel(view jobView, now time.Time) string {
	return strings.Join(append(append([]string{view.Name}, spokenStatus(view, now)...), spokenEdges(view)...), ", ")
}

// spokenStatus is what a node's status line and ring say, in words.
func spokenStatus(view jobView, now time.Time) []string {
	word, _ := nodeStatus(view)
	parts := []string{word}

	if view.HasRun && !view.Latest.StartedAt.IsZero() {
		_, spoken := agoCompact(now.Sub(view.Latest.StartedAt))
		if finished(view.Latest.Status) {
			parts[0] += " " + spoken
		} else {
			parts = append(parts, "started "+spoken)
		}
	}

	if run, ok := lastFinished(view); ok && !finished(view.Latest.Status) {
		parts = append(parts, "last finished "+statusWord(run.Status))
	}

	if view.Held {
		parts = append(parts, "held")
	}

	if queued := queuedBehind(view); queued != nil {
		waiting := "starting"
		if queued.WaitingOn != "" {
			waiting = "waiting: " + queued.WaitingOn
		}

		parts = append(parts, "queued", waiting)
	}

	return parts
}

// spokenEdges is what a node's edges say, in words.
func spokenEdges(view jobView) []string {
	var parts, triggers, reads []string

	for _, input := range view.Inputs {
		name := input.Resource
		if len(input.Passed) > 0 {
			name += " once it has passed " + spokenList(input.Passed)
		}

		if input.Trigger {
			triggers = append(triggers, name)
		} else {
			reads = append(reads, name)
		}
	}

	if len(triggers) > 0 {
		parts = append(parts, "triggered by "+spokenList(triggers))
	}

	if len(reads) > 0 {
		parts = append(parts, "reads "+spokenList(reads))
	}

	if len(view.Outputs) > 0 {
		parts = append(parts, "writes "+spokenList(view.Outputs))
	}

	return parts
}

// spokenList joins names the way a sentence does: "a", "a and b", "a, b and c".
func spokenList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}

	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// finishedClass colors a node by its latest FINISHED run — Concourse's
// dashboard rule, the one pipelineMark folds by — so a red job being rebuilt
// stays red under its running ring.
func finishedClass(view jobView) string {
	if run, ok := lastFinished(view); ok {
		return "st-" + statusWord(run.Status)
	}

	if !view.HasRun && view.Queued != nil {
		return "st-queued"
	}

	return "st-none"
}

func lastFinished(view jobView) (store.RunRow, bool) {
	if view.HasFinished {
		return view.Finished, true
	}

	if view.HasRun && finished(view.Latest.Status) {
		return view.Latest, true
	}

	return store.RunRow{}, false
}

// nodeStatus is the graph's reading of a job's latest run: the shared status
// word and its glyph. The node's color is finishedClass's, not this.
func nodeStatus(view jobView) (word, glyph string) {
	if !view.HasRun && view.Queued != nil {
		return "queued", "○"
	}

	if !view.HasRun {
		return "never ran", "○"
	}

	word = statusWord(view.Latest.Status)

	// One glyph vocabulary: the browser tab's statusMark, plus the graph's
	// own fallback for the words the tab never shows (pending).
	if glyph = statusMark(word); glyph == "" {
		glyph = "○"
	}

	return word, glyph
}

// queuedSuffix is what a queued run adds to a node's status line, counted for its width.
const queuedSuffix = " · ○ queued"

// queuedBehind is the run queued behind a job's latest one, nil when there is none or when it is the only thing the node has to say.
func queuedBehind(view jobView) *queuedJob {
	if !view.HasRun {
		return nil
	}

	return view.Queued
}
