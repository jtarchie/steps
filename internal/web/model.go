package web

// Turning stored rows into what a page shows. The run transcript itself is
// folded by internal/runview, shared with the terminal; what is here is the
// web's own additions on top of it — spend, placements, the job board.

import (
	"github.com/jtarchie/steps/internal/runview"
)

// The fold's types under the names this package has always used for them.
type (
	stepView   = runview.Step
	stepChange = runview.Change
	runFolder  = runview.Folder
)

// turnCtx is what the turn template is invoked with: the turn, whether its
// request was slow for its step — a question only the step can answer, asked
// here so a streamed turn and a drawn one ask it the same way — and the page,
// because a result's body is fetched from a URL scoped by its pipeline and run.
type turnCtx struct {
	runview.Turn
	Page map[string]any
	Slow bool
	// Pending marks the synthetic turn for a request the step ended waiting on.
	Pending bool
}

func newTurnCtx(page map[string]any, step *stepView, turn runview.Turn) turnCtx {
	return turnCtx{Turn: turn, Page: page, Slow: step.Slow(turn.Took)}
}

// waitCtx is one approval or question and the page drawing it, for the cards both the tabs and a run page draw.
type waitCtx struct {
	Nav  navData
	Item any
}

// stepCtx is what the recursive step template is invoked with: the page it is being drawn on, and the step to draw.
type stepCtx struct {
	Page map[string]any
	Step *stepView
	// OOB marks the ROOT of a fragment the live stream is sending, so it
	// carries the attribute that swaps it over the element already on the
	// page — the row for the `step` template, the head for `stephead`. Set
	// only on the outermost element: everything rendered under it is inside
	// that fragment, and htmx lifts any nested element carrying the attribute
	// OUT of the fragment to swap it on its own, which would leave the parent
	// morphed without it.
	OOB bool
}
