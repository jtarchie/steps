package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestMarkDiscsRankByHowLoudlyTheyClaimAReader: folding two scopes keeps the
// louder disc. One row per adjacent pair, so swapping any two ranks fails.
func TestMarkDiscsRankByHowLoudlyTheyClaimAReader(t *testing.T) {
	t.Parallel()

	order := []disc{discNone, discQueued, discAborted, discPassed, discPaused, discFailed, discErrored}

	for i := 1; i < len(order); i++ {
		quiet, loud := mark{Disc: order[i-1]}, mark{Disc: order[i]}

		if got := quiet.fold(loud).Disc; got != loud.Disc {
			t.Errorf("%q folded with %q kept %q", quiet.Glyph(), loud.Glyph(), mark{Disc: got}.Glyph())
		}

		if got := loud.fold(quiet).Disc; got != loud.Disc {
			t.Errorf("%q folded into %q kept %q", quiet.Glyph(), loud.Glyph(), mark{Disc: got}.Glyph())
		}
	}
}

func TestMarkFoldSumsWhatWaitsAndKeepsAnyRing(t *testing.T) {
	t.Parallel()

	folded := mark{Needs: 2}.fold(mark{Ring: true, Needs: 3}).fold(mark{})

	if folded.Needs != 5 || !folded.Ring {
		t.Errorf("folded = %+v, want 5 waiting and the ring", folded)
	}
}

func TestMarkTitlePrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mark mark
		want string
	}{
		{mark{}, ""},
		{mark{Disc: discFailed}, "✗ "},
		{mark{Ring: true}, "◐ "},
		{mark{Disc: discFailed, Ring: true}, "✗◐ "},
		{mark{Disc: discPassed, Needs: 2}, "(2) ✓ "},
		{mark{Needs: 1}, "(1) "},
	} {
		if got := tc.mark.TitlePrefix(); got != tc.want {
			t.Errorf("%+v.TitlePrefix() = %q, want %q", tc.mark, got, tc.want)
		}
	}
}

// TestMarkFavicon reads the icon's parts back out of its URI: the disc's
// color, the ring only while something runs, and a needs-you count that
// takes the disc yellow and caps at 9+.
func TestMarkFavicon(t *testing.T) {
	t.Parallel()

	const ring = "fill=%27none%27%20stroke=%27%23d9a94a%27"

	for _, tc := range []struct {
		name    string
		mark    mark
		want    []string
		notWant []string
	}{
		{"failed", mark{Disc: discFailed}, []string{"fill=%27%23e0645a%27"}, []string{ring, "%3Ctext"}},
		{"passed and running", mark{Disc: discPassed, Ring: true}, []string{ring, "fill=%27%2384c06d%27"}, nil},
		{"paused", mark{Disc: discPaused}, []string{"fill=%27%237aa4d9%27"}, nil},
		{"waiting", mark{Disc: discFailed, Needs: 3}, []string{"fill=%27%23d9a94a%27", "%3E3%3C/text%3E"}, []string{"%23e0645a"}},
		{"overflowing", mark{Needs: 12}, []string{"%3E9+%3C/text%3E"}, []string{"12"}},
	} {
		icon := string(tc.mark.Favicon())

		if !strings.HasPrefix(icon, "data:image/svg+xml,") {
			t.Errorf("%s: %q is not an svg data uri", tc.name, icon)
		}

		for _, want := range tc.want {
			if !strings.Contains(icon, want) {
				t.Errorf("%s: icon lacks %s:\n%s", tc.name, want, icon)
			}
		}

		for _, unwanted := range tc.notWant {
			if strings.Contains(icon, unwanted) {
				t.Errorf("%s: icon carries %s:\n%s", tc.name, unwanted, icon)
			}
		}
	}
}

// TestARedJobBeingRebuiltStaysRed is Concourse's dashboard rule: a job is
// colored by its latest FINISHED run, so a fix in flight reads as red AND
// running rather than merely busy.
func TestARedJobBeingRebuiltStaysRed(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	ctx := context.Background()

	finishedRun(t, pipeline, "b-1", "build", "failed")

	err := pipeline.Store.StartRun(ctx, "b-2", "build", "", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	got := pipelineMark(ctx, pipeline, 0)
	if got.Disc != discFailed || !got.Ring {
		t.Errorf("rebuilding a red job reads %q, want ✗◐", got.Glyph())
	}

	err = pipeline.Store.FinishRun(ctx, "b-2", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	if got := pipelineMark(ctx, pipeline, 0); got.Disc != discPassed || got.Ring {
		t.Errorf("the fix landing reads %q, want ✓", got.Glyph())
	}
}

// TestAJobGoneFromTheConfigIsNotFolded: its last failure would otherwise be
// a red the pipeline can never clear.
func TestAJobGoneFromTheConfigIsNotFolded(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	finishedRun(t, pipeline, "b-1", "build", "succeeded")
	finishedRun(t, pipeline, "r-1", "retired", "failed")

	if got := pipelineMark(context.Background(), pipeline, 0); got.Disc != discPassed {
		t.Errorf("a removed job's failure reads %q, want ✓", got.Glyph())
	}
}

// TestPausedSitsBetweenGreenAndRed: a paused pipeline is not moving, which
// outranks green — but a failure under it still shows through.
func TestPausedSitsBetweenGreenAndRed(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	ctx := context.Background()

	finishedRun(t, pipeline, "b-1", "build", "succeeded")

	err := pipeline.Store.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if got := pipelineMark(ctx, pipeline, 0); got.Disc != discPaused {
		t.Errorf("a paused green pipeline reads %q, want ⏸", got.Glyph())
	}

	finishedRun(t, pipeline, "d-1", "deploy", "failed")

	if got := pipelineMark(ctx, pipeline, 0); got.Disc != discFailed {
		t.Errorf("a paused pipeline with a red job reads %q, want ✗", got.Glyph())
	}
}

func finishedRun(t *testing.T, pipeline *Pipeline, id, job, status string) {
	t.Helper()

	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, id, job, "", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipeline.Store.FinishRun(ctx, id, status)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

// TestTheDoneFrameCarriesTheFinishedMark: a backgrounded tab may defer the
// closing reload indefinitely, so the outcome reaches its title and icon
// through the frame — rendered by the server, the same mark a reload draws.
func TestTheDoneFrameCarriesTheFinishedMark(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	finishedRun(t, pipeline, "b-bad", "build", "failed")

	body := streamOf(t, server, "/p/demo/runs/b-bad/events")

	_, page := get(t, server, "/p/demo/runs/b-bad")

	at := strings.Index(body, "event: done\ndata: ")
	if at < 0 {
		t.Fatalf("no done frame in:\n%s", body)
	}

	var done struct {
		Icon  string `json:"icon"`
		Title string `json:"title"`
	}

	err := json.Unmarshal([]byte(strings.SplitN(body[at+len("event: done\ndata: "):], "\n", 2)[0]), &done)
	if err != nil {
		t.Fatalf("done frame: %v", err)
	}

	if want := runMark(store.RunRow{Status: "failed"}).Favicon(); done.Icon != string(want) {
		t.Errorf("done icon = %q, want the failed run's %q", done.Icon, want)
	}

	if !strings.Contains(page, "<title>"+html.EscapeString(done.Title)+"</title>") {
		t.Errorf("done title %q is not the title the reloaded page draws", done.Title)
	}
}

var carrierTag = regexp.MustCompile(`<span id="mark"[^>]*></span>`)

// TestAHiddenTabAsksForWhatThePageWouldDraw: the mark route is the page's
// own #mark and nothing else, so a tab refreshed while hidden ends up exactly
// where a visible poll would have put it.
func TestAHiddenTabAsksForWhatThePageWouldDraw(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")

	// Fixed since: the pipeline is green, so only the run's own route can
	// still answer red.
	finishedRun(t, pipelines[0], "b-bad", "app-job", "failed")
	finishedRun(t, pipelines[0], "b-ok", "app-job", "succeeded")

	for _, tc := range []struct{ page, route, color string }{
		{"/", "/mark", "%2384c06d"},
		{"/p/app", "/p/app/mark", "%2384c06d"},
		{"/p/app/runs/b-bad", "/p/app/runs/b-bad/mark", "%23e0645a"},
	} {
		_, page := get(t, server, tc.page)

		drawn := carrierTag.FindString(page)
		if !strings.Contains(drawn, `hx-get="`+tc.route+`"`) {
			t.Errorf("%s: carrier %q does not ask %s", tc.page, drawn, tc.route)
		}

		code, answer := get(t, server, tc.route)
		if code != http.StatusOK || strings.TrimSpace(answer) != drawn {
			t.Errorf("%s answered %d:\n%s\nwant the page's own carrier:\n%s", tc.route, code, answer, drawn)
		}

		if !strings.Contains(drawn, tc.color) {
			t.Errorf("%s: carrier %q is not colored %s", tc.page, drawn, tc.color)
		}
	}
}

// TestTheCarrierPollsOnlyWhileHidden pins the spelling, which is the whole
// behavior: htmx 4 reads `every 30s` as a nested path (and polls at
// milliseconds), and a filter detached from the event name is dropped.
// Visible, the page's own poll carries #mark; hidden, only this asks.
func TestTheCarrierPollsOnlyWhileHidden(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, page := get(t, server, "/p/demo")
	drawn := carrierTag.FindString(page)

	for _, want := range []string{
		`hx-trigger="every[document.hidden] 30000ms"`,
		`hx-swap="outerMorph"`,
		`hx-status:4xx="swap:none"`,
		`hx-status:5xx="swap:none"`,
	} {
		if !strings.Contains(drawn, want) {
			t.Errorf("carrier lacks %s:\n%s", want, drawn)
		}
	}

	_, docs := get(t, server, "/docs/web.md")
	if quiet := carrierTag.FindString(docs); quiet == "" || strings.Contains(quiet, "hx-get") {
		t.Errorf("a page about no pipeline carries %q, want a carrier that never polls", quiet)
	}
}

// TestAMarkRouteAnswersNothingForWhatIsGone: htmx swaps error bodies too,
// and the carrier declines them — but a body would still be rendered and
// sent for nobody, and an error PAGE over the carrier is what kills its poll.
func TestAMarkRouteAnswersNothingForWhatIsGone(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, route := range []string{"/p/demo/runs/nosuch/mark", "/p/nosuch/mark"} {
		code, body := get(t, server, route)
		if code != http.StatusNotFound || body != "" {
			t.Errorf("%s = %d %q, want an empty 404", route, code, body)
		}
	}
}
