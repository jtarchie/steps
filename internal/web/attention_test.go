package web

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// writablePipeline is testPipeline with a runner, so the controls a read-only
// daemon withholds are on the page.
func writablePipeline(t *testing.T) (*Server, *Pipeline) {
	t.Helper()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipeline
}

// TestTheHeaderSaysWhenSomethingIsWaitingOnYou is the whole feature in one
// page load: four unrelated things are stuck, and a reader who opened the
// jobs board and nothing else has to be able to see all four and reach the
// page that fixes each.
//
// Before this, two of the four were invisible anywhere but their own tab and
// a third was invisible entirely.
func TestTheHeaderSaysWhenSomethingIsWaitingOnYou(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	_, err := pipeline.Store.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	startRunningBuild(t, pipeline)

	_, _, err = pipeline.Store.AskQuestion(ctx, store.Question{
		RunID: "run-1", JobName: "build", AgentName: "review", Question: "which branch?",
	})
	if err != nil {
		t.Fatalf("AskQuestion: %v", err)
	}

	for _, job := range []string{"build", "deploy"} {
		_, _, err = pipeline.Store.RecordJobOutcome(ctx, job, false, 1)
		if err != nil {
			t.Fatalf("RecordJobOutcome %s: %v", job, err)
		}
	}

	err = pipeline.Store.RecordCheckError(ctx, "repo", "check: exit status 128")
	if err != nil {
		t.Fatalf("RecordCheckError: %v", err)
	}

	_, body := get(t, server, "/p/demo")

	for _, want := range []string{
		`id="attention"`,
		"2 jobs paused after repeated failures",
		"1 resource is failing its check",
		"1 approval is waiting for a decision",
		"1 question is waiting for an answer",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the jobs board never says %q", want)
		}
	}

	for _, want := range []string{
		`href="/p/demo/resources"`,
		`href="/p/demo/approvals"`,
		`href="/p/demo/questions"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("nothing links %s", want)
		}
	}
}

// TestEachBadgeSitsOnTheTabThatFixesIt: a count is only useful where it is
// also a route. Reading it on the wrong tab costs the reader the click it was
// supposed to save.
func TestEachBadgeSitsOnTheTabThatFixesIt(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	_, _, err := pipeline.Store.RecordJobOutcome(ctx, "build", false, 1)
	if err != nil {
		t.Fatalf("RecordJobOutcome: %v", err)
	}

	err = pipeline.Store.RecordCheckError(ctx, "repo", "boom")
	if err != nil {
		t.Fatalf("RecordCheckError: %v", err)
	}

	_, body := get(t, server, "/p/demo")

	for _, want := range []string{
		`>jobs<span class="badge"`,
		`>resources<span class="badge"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no badge on the tab for %q:\n%s", want, body)
		}
	}
}

// TestNothingWaitingDrawsNothing: the surface's presence IS the signal, so it
// must have no resting state. A banner that is always there, empty, is a
// banner nobody reads when it fills.
func TestNothingWaitingDrawsNothing(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo")

	if strings.Contains(body, `class="badge"`) {
		t.Error("a clean pipeline draws a badge")
	}

	if strings.Contains(body, `class="attention"`) {
		t.Error("a clean pipeline draws the attention list")
	}
}

// TestAPausedPipelineIsTheFirstThingInTheList: it outranks everything else on
// the page because it is the reason none of the rest will resolve on its own,
// and it keeps the inline unpause it had as a banner of its own.
func TestAPausedPipelineIsTheFirstThingInTheList(t *testing.T) {
	t.Parallel()

	server, pipeline := writablePipeline(t)
	ctx := t.Context()

	_, err := pipeline.Store.RequestApproval(ctx, "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	err = pipeline.Store.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, body := get(t, server, "/p/demo")
	list := attentionList(t, body)

	paused := strings.Index(list, "this pipeline is paused")
	approval := strings.Index(list, "1 approval is waiting")

	if paused < 0 || approval < 0 {
		t.Fatalf("attention list is missing an item:\n%s", list)
	}

	if paused > approval {
		t.Error("a paused pipeline is listed below a waiting approval")
	}

	if !strings.Contains(list, `action="/p/demo/unpause"`) {
		t.Error("the paused item lost its inline unpause")
	}
}

// attentionList is the <ul> and nothing else. The sentences also travel as
// the tab badges' title attributes, which sit ABOVE the list in the document
// — so an unscoped search finds the header's copy and answers an ordering
// question with the order of the tabs.
func attentionList(t *testing.T, body string) string {
	t.Helper()

	return between(t, body, `<ul class="attention"`, "</ul>")
}

// between is the markup from one marker up to the next, failing the test when
// either is missing rather than slicing on a -1.
func between(t *testing.T, body, opening, closing string) string {
	t.Helper()

	from := strings.Index(body, opening)
	if from < 0 {
		t.Fatalf("page has no %s:\n%s", opening, body)
	}

	rest := body[from:]

	to := strings.Index(rest, closing)
	if to < 0 {
		t.Fatalf("%s is never closed by %s:\n%s", opening, closing, rest)
	}

	return rest[:to]
}

// TestTheSwitcherMarksAPipelineYouAreNotLookingAt: a daemon holds several,
// and a reader looking at a clean one would otherwise have no way to learn
// that another has stopped short of opening it.
func TestTheSwitcherMarksAPipelineYouAreNotLookingAt(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "alpha", "beta")

	_, err := pipelines[1].Store.RequestApproval(t.Context(), "beta-job", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	_, body := get(t, server, "/p/alpha")

	menu := between(t, body, `id="pipemenu"`, "</div>")

	if !strings.Contains(menu, `class="badge"`) {
		t.Errorf("the switcher does not mark beta:\n%s", menu)
	}
}

// TestAnMCPServerNeedingALoginReachesTheHeader: the mcp tab already sorts what
// needs a person to the top of its own page — which only helps somebody who
// had a reason to open it.
func TestAnMCPServerNeedingALoginReachesTheHeader(t *testing.T) {
	t.Parallel()

	server, _, _ := mcpServerPipeline(t, nil)

	_, body := get(t, server, "/p/demo")

	if !strings.Contains(body, "mcp servers need a connection") {
		t.Errorf("the header says nothing about the servers the mcp tab calls broken:\n%s", body)
	}

	if !strings.Contains(body, `>mcp<span class="badge"`) {
		t.Error("no badge on the mcp tab")
	}
}

// TestTheBannerLandsSomewhereThatExplains: "1 resource is failing its check"
// links to the resources page, and that page owes the reader the reason. Sent
// to a table of timestamps that looks fine, they learn nothing the header had
// not already told them.
func TestTheBannerLandsSomewhereThatExplains(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	err := pipeline.Store.RecordCheckError(t.Context(), "repo", "trigger resource \"repo\": dial tcp: connection refused")
	if err != nil {
		t.Fatalf("RecordCheckError: %v", err)
	}

	for _, page := range []string{"/p/demo/resources", "/p/demo/resources/repo"} {
		_, body := get(t, server, page)

		if !strings.Contains(body, "dial tcp: connection refused") {
			t.Errorf("%s does not say why the check failed:\n%s", page, body)
		}
	}
}

// TestAFailingCheckDoesNotPassItselfOffAsAFreshOne: the recorded version is
// what the last SUCCESSFUL check saw, and a failing one leaves it exactly
// where it was — so a "checked 4s ago" beside it is a lie the row tells on
// every poll.
func TestAFailingCheckDoesNotPassItselfOffAsAFreshOne(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.RecordCheckedVersion(ctx, "repo", `{"ref":"abc"}`)
	if err != nil {
		t.Fatalf("RecordCheckedVersion: %v", err)
	}

	err = pipeline.Store.RecordCheckError(ctx, "repo", "exit status 128")
	if err != nil {
		t.Fatalf("RecordCheckError: %v", err)
	}

	_, body := get(t, server, "/p/demo/resources")

	if !strings.Contains(body, `<span class="st st-failed">failing</span>`) {
		t.Errorf("the checked column does not mark the resource as failing:\n%s", body)
	}
}

// TestTheOverviewRanksPipelinesByWhatTheyWant: the root is the one page that
// can put several pipelines beside each other, so the question the header
// answers for one of them is a column here.
func TestTheOverviewRanksPipelinesByWhatTheyWant(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "alpha", "beta")

	_, err := pipelines[1].Store.RequestApproval(t.Context(), "beta-job", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	_, body := get(t, server, "/")

	if !strings.Contains(body, ">Waiting<") {
		t.Errorf("the overview has no waiting column:\n%s", body)
	}

	if !strings.Contains(body, `<a class="badge" href="/p/beta">`) {
		t.Errorf("the overview does not mark beta as waiting:\n%s", body)
	}

	if strings.Contains(body, `<a class="badge" href="/p/alpha">`) {
		t.Error("the overview marks a clean pipeline as waiting")
	}
}
