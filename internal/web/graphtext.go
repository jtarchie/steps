package web

import (
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// nodeTitle is the detail a node's two lines leave out — what the list view's
// duration and held columns said before the graph was the only view.
func nodeTitle(loc *locale, view jobView) string {
	var parts []string

	if view.HasRun {
		key := "graph.title.took"
		if !finished(view.Latest.Status) {
			key = "graph.title.runningfor"
		}

		parts = append(parts, loc.text(key, formatDuration(view.Latest.Duration())))
	}

	if view.Held {
		if view.HeldAt.IsZero() {
			parts = append(parts, loc.count("graph.title.held", view.Failures))
		} else {
			parts = append(parts, loc.count("graph.title.heldago", view.Failures, formatAgoIn(loc, view.HeldAt)))
		}
	}

	if view.Queued != nil {
		parts = append(parts, queuedWords(loc, view.Queued.WaitingOn))
	}

	return strings.Join(parts, loc.text("graph.join"))
}

// queuedWords is "a run is queued" and what it waits on, whole, so a language can order it its own way.
func queuedWords(loc *locale, waitingOn string) string {
	if waitingOn == "" {
		return loc.text("graph.title.queued.starting")
	}

	return loc.text("graph.title.queued.waiting", waitingOn)
}

// jobLabel is a job node's accessible name: "draft, passed 15 hours ago,
// triggered by weekday, writes draft".
func jobLabel(loc *locale, view jobView, now time.Time) string {
	parts := append(append([]string{view.Name}, spokenStatus(loc, view, now)...), spokenEdges(loc, view)...)
	if line := mcpLine(loc, view); line != "" {
		parts = append(parts, line)
	}

	return strings.Join(parts, loc.text("graph.label.join"))
}

// spokenStatus is what a node's status line and ring say, in words.
func spokenStatus(loc *locale, view jobView, now time.Time) []string {
	word, _ := nodeStatus(view)
	parts := []string{loc.word("status", word)}

	if view.HasRun && !view.Latest.StartedAt.IsZero() {
		_, spoken := agoCompactIn(loc, now.Sub(view.Latest.StartedAt))
		if finished(view.Latest.Status) {
			parts[0] = loc.text("graph.spoken.status", parts[0], spoken)
		} else {
			parts = append(parts, loc.text("graph.spoken.started", spoken))
		}
	}

	if run, ok := lastFinished(view); ok && !finished(view.Latest.Status) {
		parts = append(parts, loc.text("graph.spoken.lastfinished", loc.word("status", statusWord(run.Status))))
	}

	if view.Held {
		parts = append(parts, loc.word("status", "held"))
	}

	if queued := queuedBehind(view); queued != nil {
		waiting := loc.text("graph.spoken.starting")
		if queued.WaitingOn != "" {
			waiting = loc.text("graph.spoken.waiting", queued.WaitingOn)
		}

		parts = append(parts, loc.word("status", "queued"), waiting)
	}

	return parts
}

// spokenEdges is what a node's edges say, in words.
func spokenEdges(loc *locale, view jobView) []string {
	var parts, triggers, reads []string

	for _, input := range view.Inputs {
		name := input.Resource
		if len(input.Passed) > 0 {
			name = loc.text("graph.spoken.passed", name, spokenList(loc, input.Passed))
		}

		if input.Trigger {
			triggers = append(triggers, name)
		} else {
			reads = append(reads, name)
		}
	}

	if len(triggers) > 0 {
		parts = append(parts, loc.text("graph.spoken.triggered", spokenList(loc, triggers)))
	}

	if len(reads) > 0 {
		parts = append(parts, loc.text("graph.spoken.reads", spokenList(loc, reads)))
	}

	if len(view.Outputs) > 0 {
		parts = append(parts, loc.text("graph.spoken.writes", spokenList(loc, view.Outputs)))
	}

	return parts
}

// spokenList joins names the way a sentence does: "a", "a and b", "a, b and c".
func spokenList(loc *locale, names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}

	return loc.text("list.and", strings.Join(names[:len(names)-1], loc.text("list.sep")), names[len(names)-1])
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
func queuedSuffix(loc *locale) string { return " · ○ " + loc.text("jobs.queued") }

// queuedBehind is the run queued behind a job's latest one, nil when there is none or when it is the only thing the node has to say.
func queuedBehind(view jobView) *queuedJob {
	if !view.HasRun {
		return nil
	}

	return view.Queued
}
