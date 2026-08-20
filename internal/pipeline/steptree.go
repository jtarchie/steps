package pipeline

// The display tree: which step a reader sees folded under which.
//
// The plan is a tree — a get: delegates the whole remainder of the plan to
// the build it triggers, an across: runs its cells, a try: wraps one step —
// but the event stream said none of that. Every step reported a plan index
// and a name, so a matrix's cells arrived as siblings of the matrix, and a
// try: and the step inside it (same index, same name) collapsed into one row.
//
// So each step occurrence is stamped with an id, and with the id of the
// container it ran inside. Two rules make that enough:
//
//   - a step MINTS its id before it runs, and
//   - it hands that id to everything that runs beneath it, on the context.
//
// The second rule is why this costs one line at each of the few places a
// container runs children, rather than a parameter through every runner: the
// containers already pass their context down.
//
// Ids are per RUN, not global: they mean nothing outside the run they were
// minted in, which is also the only place anything reads them.

import "context"

// stepMark is one step occurrence's place in the tree — its own id, and the
// container it belongs to. Carried from a step's start event to its finish
// event so both report the same identity.
type stepMark struct {
	id     int64
	parent int64
}

type parentStepKey struct{}

// markStep mints the next id for this run and pairs it with whatever
// container the context is currently inside.
//
// Outside a run — a hook or a fix conversation, which have no plan and no
// resume state — there is nothing to mint from, and the zero mark is
// correct: those publish no tree because they are not in one.
func markStep(ctx context.Context) stepMark {
	mark := stepMark{parent: parentStepFrom(ctx)}

	if resume := resumeFrom(ctx); resume != nil {
		mark.id = resume.nextStepID.Add(1)
	}

	return mark
}

// withChildrenOf returns a context whose steps report mark as their parent.
// Called by a container before it runs what it contains.
func withChildrenOf(ctx context.Context, mark stepMark) context.Context {
	if mark.id == 0 {
		return ctx
	}

	return context.WithValue(ctx, parentStepKey{}, mark.id)
}

// parentStepFrom reads the container a context is inside, 0 at the top of a
// plan.
func parentStepFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(parentStepKey{}).(int64)

	return id
}
