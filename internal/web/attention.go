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

import (
	"context"
	"fmt"
)

// attentionItem is one thing waiting on a person.
type attentionItem struct {
	// Kind is what it is, which the template needs because one item — the
	// paused pipeline — is fixed by a button here rather than by a page
	// somewhere else.
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
// The order is by blast radius rather than recency: a paused pipeline is the
// reason none of the rest will resolve on its own, so it leads, and the two
// items that are one person away bring up the rear.
//
// Every probe answers nil rather than an error, and a read that fails answers
// nil too. This runs on every render of every page, including the 2.5s
// self-poll — a store hiccup must cost a line of the banner, never the page.
func (s *Server) attention(ctx context.Context, target *Pipeline) []attentionItem {
	if target == nil || target.Store == nil {
		return nil
	}

	base := "/p/" + target.Slug

	found := []*attentionItem{
		pausedItem(ctx, target),
		pausedJobsItem(ctx, target, base),
		failingChecksItem(ctx, target, base),
		s.mcpItem(target, base),
		approvalsItem(ctx, target, base),
		questionsItem(ctx, target, base),
	}

	items := make([]attentionItem, 0, len(found))

	for _, item := range found {
		if item != nil {
			items = append(items, *item)
		}
	}

	return items
}

// pausedItem is the one item no tab owns, and the only one fixed by a button
// on the page reporting it.
func pausedItem(ctx context.Context, target *Pipeline) *attentionItem {
	if !paused(ctx, target) {
		return nil
	}

	return &attentionItem{Kind: "paused", Count: 1, Detail: "this pipeline is paused"}
}

// pausedJobsItem: the breaker took these out of rotation on their own, and
// nothing puts them back without a person.
func pausedJobsItem(ctx context.Context, target *Pipeline, base string) *attentionItem {
	jobs, err := target.Store.PausedJobs(ctx)
	if err != nil || len(jobs) == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "jobs", Tab: "jobs", Count: len(jobs), URL: base,
		Detail: countOf(len(jobs), "job", "jobs") + " paused after repeated failures",
	}
}

// failingChecksItem names the CONSEQUENCE, not the row: a poll aborts on the
// first resource that errors, so one broken check stops the pipeline
// triggering at all — which is the part a reader could not have guessed.
func failingChecksItem(ctx context.Context, target *Pipeline, base string) *attentionItem {
	failing, err := target.Store.CheckErrors(ctx)
	if err != nil || len(failing) == 0 {
		return nil
	}

	detail := "1 resource is failing its check — nothing is polled until it succeeds"
	if len(failing) > 1 {
		detail = fmt.Sprintf("%d resources are failing their checks — nothing is polled until they succeed", len(failing))
	}

	return &attentionItem{
		Kind: "checks", Tab: "resources", Count: len(failing), URL: base + "/resources", Detail: detail,
	}
}

// mcpItem counts the declared servers the mcp tab would sort to the top of its
// own page.
//
// It asks mcpRowFor rather than re-deriving the condition, because "cannot be
// used right now" is a judgment that took three sources to settle (the
// configuration, this machine, and whoever holds the token) and a second copy
// of it would drift from the page a reader is sent to.
func (s *Server) mcpItem(target *Pipeline, base string) *attentionItem {
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
		Detail: countOf(needing, "mcp server needs", "mcp servers need") + " a connection",
	}
}

// approvalsItem: a run is parked and a person is the gate.
func approvalsItem(ctx context.Context, target *Pipeline, base string) *attentionItem {
	pending, err := target.Store.Approvals(ctx, true, 0)
	if err != nil || len(pending) == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "approvals", Tab: "approvals", Count: len(pending), URL: base + "/approvals",
		Detail: countOf(len(pending), "approval is", "approvals are") + " waiting for a decision",
	}
}

// questionsItem: an agent is parked on an answer.
func questionsItem(ctx context.Context, target *Pipeline, base string) *attentionItem {
	pending, err := target.Store.Questions(ctx, true, 0)
	if err != nil || len(pending) == 0 {
		return nil
	}

	return &attentionItem{
		Kind: "questions", Tab: "questions", Count: len(pending), URL: base + "/questions",
		Detail: countOf(len(pending), "question is", "questions are") + " waiting for an answer",
	}
}

// countOf writes "1 job" or "4 jobs", the caller supplying both words because
// English pluralizes the verb too ("approval is", "approvals are").
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}

	return fmt.Sprintf("%d %s", n, many)
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
