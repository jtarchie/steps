package web

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/jtarchie/steps/internal/store"
)

// overflowOf is how far each table's frame, then the page, scrolls sideways: positive is a column or a button the reader has to hunt for behind a scrollbar.
const overflowOf = `(() => [...document.querySelectorAll('.tablewrap')]
  .map(w => w.scrollWidth - w.clientWidth)
  .concat([document.documentElement.scrollWidth - innerWidth]))()`

const longSHA = "4e1b8734ed0ec76b5d27e454a9122f11a640536a"

// listServer serves the lists with the content that broke them: a sha that will not wrap, reasons and answers that are sentences, and controls on every row.
func listServer(t *testing.T) string {
	t.Helper()

	_, pipeline := testPipeline(t)
	ctx := t.Context()

	_, err := pipeline.Store.RecordVersions(ctx, "repo", []map[string]any{{"sha": longSHA}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	key, err := store.EncodeVersion(map[string]any{"sha": longSHA})
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.RecordCheckedVersion(ctx, "repo", key)
	if err != nil {
		t.Fatal(err)
	}

	// What a PR resource says about a version: an unbreakable URL and a sentence of title are what squeezed the version column on a phone.
	err = pipeline.Store.RecordVersionMetadata(ctx, "repo", key, `[{"name":"url","value":"https://github.com/example/app/pull/23431"},{"name":"title","value":"Use the crowdfunding flash toast app-wide and share it, the pager, icons and helpers"},{"name":"author","value":"someone-with-a-long-name"}]`)
	if err != nil {
		t.Fatal(err)
	}

	startFinishedRun(t, pipeline, "RUNONE", "build", "succeeded")

	id, err := pipeline.Store.RequestApproval(ctx, "deploy", "Post the release notes for the whole quarter to Linear?")
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.DecideApproval(ctx, id, store.ApprovalRejected, "someone-with-a-long-name", "two tickets are mislabelled — fix the labels before anything is posted")
	if err != nil {
		t.Fatal(err)
	}

	question := askOne(t, pipeline)

	err = pipeline.Store.AnswerQuestion(ctx, question.ID, "minor, because nothing in the diff changes a public signature", "someone-with-a-long-name")
	if err != nil {
		t.Fatal(err)
	}

	pipeline.CheckNow = func(string) bool { return true }

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatal(err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	return listener.URL
}

// A list's table fits its frame with its buttons showing on a phone, a tablet and a desktop: the bug this page set was a Check or Re-connect button pushed off the right edge behind a scrollbar, and it came back at 768px after being fixed at 1280px.
func TestListTablesFitEveryScreenWithTheirButtons(t *testing.T) {
	ctx := browser(t)

	lists := listServer(t)

	mcpServer, _, _ := mcpServerPipeline(t, stubRunner{})
	mcpListener := httptest.NewServer(mcpServer.Handler())
	t.Cleanup(mcpListener.Close)

	pages := []string{
		lists + "/p/demo/runs",
		lists + "/p/demo/resources",
		lists + "/p/demo/resources/repo",
		lists + "/p/demo/approvals",
		lists + "/p/demo/questions",
		mcpListener.URL + "/p/demo/mcp",
	}

	for _, size := range []struct {
		name string
		view chromedp.Action[chromedp.Void]
	}{
		{"phone", onPhone()},
		{"tablet", chromedp.EmulateViewport(768, 1024)},
		{"desktop", chromedp.EmulateViewport(1280, 800)},
	} {
		for _, page := range pages {
			over := must(ctx, t, chromedp.Evaluate[[]float64](overflowOf), size.view, media("dark", false), chromedp.Navigate(page))

			for i, px := range over {
				if px > 0 {
					what := fmt.Sprintf("table %d", i)
					if i == len(over)-1 {
						what = "the page"
					}

					t.Errorf("%s at %s: %s scrolls sideways by %.0fpx", page, size.name, what, px)
				}
			}
		}
	}
}

// A card on a phone has no room for a lone dash: a never-checked resource's empty version and checked cells vanish there, and stay on a desktop, where the dash keeps a column honest.
func TestAPhoneDropsEmptyCellsAndADesktopKeepsThem(t *testing.T) {
	ctx := browser(t)

	// Unseeded: the pipeline's one resource has never been checked, so its version and checked cells are empty.
	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, stubRunner{})
	if err != nil {
		t.Fatal(err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	page := listener.URL + "/p/demo/resources"

	const visibleEmpty = `[...document.querySelectorAll('td.empty')].filter(td => td.offsetParent !== null).length`

	phone := must(ctx, t, chromedp.Evaluate[int](visibleEmpty), onPhone(), media("dark", false), chromedp.Navigate(page))
	desktop := must(ctx, t, chromedp.Evaluate[int](visibleEmpty), chromedp.EmulateViewport(1280, 800), media("dark", false), chromedp.Navigate(page))

	if phone != 0 {
		t.Errorf("a phone shows %d empty cells, want none", phone)
	}

	if desktop == 0 {
		t.Error("a desktop shows no empty cells; the dash that keeps a never-checked row's columns aligned is gone")
	}
}

// On a phone a version row is a card: the version and what its resource said about it take the whole width, rather than a fifth of a four-column grid where a PR title wrapped a letter or two per line.
func TestAPhoneReadsAVersionAsACard(t *testing.T) {
	ctx := browser(t)

	lists := listServer(t)

	const measure = `(() => {
		const th = document.querySelector('table.versions tbody th');
		const table = document.querySelector('table.versions');
		return [th.getBoundingClientRect().width, table.getBoundingClientRect().width];
	})()`

	widths := must(ctx, t, chromedp.Evaluate[[]float64](measure), onPhone(), media("dark", false), chromedp.Navigate(lists+"/p/demo/resources/repo"))
	if len(widths) != 2 || widths[0] < widths[1]*0.9 {
		t.Errorf("on a phone the version takes %v of the row's px, want nearly all of it", widths)
	}
}

// A version is JSON and reads as JSON everywhere else in the UI: its key and its value keep their own colors rather than one dim gray.
func TestAVersionKeepsItsJSONColors(t *testing.T) {
	ctx := browser(t)

	lists := listServer(t)

	const colors = `(() => {
		const th = document.querySelector('table.versions tbody th');
		return [getComputedStyle(th.querySelector('.j-key')).color, getComputedStyle(th.querySelector('.j-str')).color];
	})()`

	got := must(ctx, t, chromedp.Evaluate[[]string](colors), media("dark", false), chromedp.Navigate(lists+"/p/demo/resources/repo"))
	if len(got) != 2 || got[0] == got[1] {
		t.Errorf("a version's key and value are both %v, want each highlighted", got)
	}
}
