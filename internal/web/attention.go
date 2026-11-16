package web

// The attention surface: everything that is STUCK and that a person reading
// this page can unstick, gathered once per render and shown in three
// densities — a count on the tab that fixes it, a sentence under the header,
// and a mark beside the other pipelines this daemon holds.
//
// Two rules decide what may be in here, and both are subtractive:
//
//   - It must be stuck. Activity is not attention. A count of running steps
//     or open resources is never zero, and a signal that is never zero is
//     read past within a week — taking the signals that DO mean something
//     with it.
//   - A person here must be able to act on it. A failed run is not in this
//     list: the board's whole job is showing failed runs, and a header that
//     only goes quiet when somebody fixes the world is a header that is
//     always loud. A deep trigger queue is not in it either — a queue held by
//     serial: looks exactly like a queue nothing is draining, and a signal
//     that cannot tell a healthy state from a stuck one manufactures alarms.
//
// What that leaves is short by construction. If this list ever needs to
// scroll, something was admitted that should not have been.

import "context"

// attentionItem is one thing waiting on a person.
type attentionItem struct {
	// Kind is what it is, for the item's class.
	Kind string
	// Tab is the nav tab whose badge carries this count, empty for an item no
	// tab owns. A count is only worth showing where it is also a route.
	Tab    string
	Count  int
	Detail string
	URL    string
}

// attention gathers one pipeline's items, most-blocking first.
//
// The order is by blast radius rather than recency: the two items that are one
// person away bring up the rear. A paused pipeline is not an item: the action
// bar says it, and carries Unpause, on every page.
//
// Every probe answers nil rather than an error, and a read that fails answers
// nil too. This runs on every render of every page, including the 2.5s
// self-poll — a store hiccup must cost a line of the banner, never the page.
func (s *Server) attention(ctx context.Context, reads *pipelineReads, loc *locale) []attentionItem {
	target := reads.target
	if target == nil || target.Store == nil {
		return nil
	}

	base := "/p/" + target.Slug

	found := []*attentionItem{
		pausedJobsItem(ctx, reads, base, loc),
		failingChecksItem(ctx, reads, base, loc),
		s.mcpItem(target, base, loc),
		approvalsItem(ctx, reads, base, loc),
		questionsItem(ctx, reads, base, loc),
	}

	items := make([]attentionItem, 0, len(found))

	for _, item := range found {
		if item != nil {
			items = append(items, *item)
		}
	}

	return items
}

// pausedJobsItem: the breaker took these out of rotation on their own, and
// nothing puts them back without a person.
func pausedJobsItem(ctx context.Context, reads *pipelineReads, base string, loc *locale) *attentionItem {
	jobs, err := reads.HeldJobs(ctx)
	if err != nil || len(jobs) == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "jobs", Tab: "jobs", Count: len(jobs), URL: base,
		Detail: loc.count("attention.held", len(jobs)),
	}
}

// failingChecksItem names the CONSEQUENCE, not the row: a poll aborts on the
// first resource that errors, so one broken check stops the pipeline
// triggering at all — which is the part a reader could not have guessed.
func failingChecksItem(ctx context.Context, reads *pipelineReads, base string, loc *locale) *attentionItem {
	failing, err := reads.CheckErrors(ctx)
	if err != nil {
		return nil
	}

	target := reads.target

	// Only what the pipeline still declares. A cleared error is a DELETE by
	// the POLLER, and nothing polls a resource a `steps pipeline set` removed
	// — so its row outlives it, and an unfiltered count would be a badge
	// nobody can ever turn off pointing at a page that lists no such
	// resource. The count has to equal what the page it links to shows.
	declared := make(map[string]bool, len(target.Config().Resources))
	for _, resource := range target.Config().Resources {
		declared[resource.Name] = true
	}

	count := 0

	for _, row := range failing {
		if declared[row.Name] {
			count++
		}
	}

	if count == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "checks", Tab: "resources", Count: count, URL: base + "/resources", Detail: loc.count("attention.checks", count),
	}
}

// mcpItem counts the declared servers the mcp tab would sort to the top of its
// own page.
//
// It asks mcpRowFor rather than re-deriving the condition, because "cannot be
// used right now" is a judgment that took three sources to settle (the
// configuration, this machine, and whoever holds the token) and a second copy
// of it would drift from the page a reader is sent to.
func (s *Server) mcpItem(target *Pipeline, base string, loc *locale) *attentionItem {
	needing := 0

	for _, server := range target.Config().MCPServers {
		if s.mcpRowFor(target, server).NeedsAttention {
			needing++
		}
	}

	if needing == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "mcp", Tab: "mcp", Count: needing, URL: base + "/mcp",
		Detail: loc.count("attention.mcp", needing),
	}
}

// approvalsItem: a run is parked and a person is the gate.
func approvalsItem(ctx context.Context, reads *pipelineReads, base string, loc *locale) *attentionItem {
	pending, err := reads.PendingApprovals(ctx)
	if err != nil || len(pending) == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "approvals", Tab: "approvals", Count: len(pending), URL: base + "/approvals",
		Detail: loc.count("attention.approvals", len(pending)),
	}
}

// questionsItem: an agent is parked on an answer.
func questionsItem(ctx context.Context, reads *pipelineReads, base string, loc *locale) *attentionItem {
	pending, err := reads.PendingQuestions(ctx)
	if err != nil || len(pending) == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "questions", Tab: "questions", Count: len(pending), URL: base + "/questions",
		Detail: loc.count("attention.questions", len(pending)),
	}
}

// attentionTotal is everything one pipeline is waiting on, which is what a
// switcher row has room for.
func attentionTotal(items []attentionItem) int {
	total := 0
	for _, item := range items {
		total += item.Count
	}

	return total
}

// attentionOn is the template's lookup: the item a tab's badge draws, or nil.
// Returned as a pointer so `{{with}}` renders nothing for a clean tab without
// the template having to ask twice.
func attentionOn(nav navData, tab string) *attentionItem {
	for index, item := range nav.Attention {
		if item.Tab == tab {
			return &nav.Attention[index]
		}
	}

	return nil
}
