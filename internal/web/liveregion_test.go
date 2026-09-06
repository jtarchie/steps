package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// The staleness probe.
//
// Every page here answers a question whose answer changes while somebody is
// looking at it: a job starts, an agent parks a question, a check records a
// version. htmx re-reads the page every 2.5s and swaps the elements named in
// hx-select and hx-select-oob, so anything that changes and is NOT inside one
// of them is a line the reader can only get by reloading — which is exactly
// how the job page sat there saying "No runs recorded yet" through a run it
// had itself triggered.
//
// The test renders each page, changes the state behind it, renders again, and
// insists every line that appeared lies inside a refreshed region. It is a
// probe for a whole class of bug rather than one instance of it: a new page,
// or a new stateful section on an old one, fails here until htmx is pointed
// at it.

// liveRegions returns the byte spans of every element htmx refreshes on this
// page, read the way htmx itself reads them: an element that polls names its
// own target in hx-select and everything else it swaps out of band in
// hx-select-oob, and both name ids.
//
// A tag-depth scan rather than a parser: internal/web renders its own
// templates, the markup is balanced, and adding an HTML parser to this
// package's dependency list to check a nesting question is a poor trade.
func liveRegions(t *testing.T, body string) [][2]int {
	t.Helper()

	var spans [][2]int

	for _, id := range refreshedIDs(body) {
		mark := strings.Index(body, `id="`+id+`"`)
		if mark < 0 {
			t.Errorf("htmx refreshes #%s, but nothing on the page has that id", id)

			continue
		}

		open := strings.LastIndex(body[:mark], "<")
		if open < 0 {
			t.Fatalf("id=%q at %d is not inside a tag", id, mark)
		}

		name := body[open+1:]
		if end := strings.IndexAny(name, " \t\r\n>"); end >= 0 {
			name = name[:end]
		}

		spans = append(spans, [2]int{open, closeOf(t, body, open, name)})
	}

	return spans
}

// refreshedIDs reads the ids named by every hx-select and hx-select-oob on
// the page.
func refreshedIDs(body string) []string {
	var ids []string

	for _, attr := range []string{` hx-select="`, ` hx-select-oob="`} {
		for at := 0; ; {
			mark := strings.Index(body[at:], attr)
			if mark < 0 {
				break
			}

			mark += at + len(attr)

			shut := strings.Index(body[mark:], `"`)
			if shut < 0 {
				break
			}

			for _, ref := range strings.Split(body[mark:mark+shut], ",") {
				ids = append(ids, strings.TrimPrefix(strings.TrimSpace(ref), "#"))
			}

			at = mark + shut
		}
	}

	return ids
}

// closeOf finds the index just past the element's closing tag, counting
// nested opens of the same name.
func closeOf(t *testing.T, body string, open int, name string) int {
	t.Helper()

	depth := 0

	for at := open; at < len(body); {
		next := strings.Index(body[at:], "<"+name)
		shut := strings.Index(body[at:], "</"+name+">")

		if shut < 0 {
			t.Fatalf("element <%s> opened at %d is never closed", name, open)
		}

		if next >= 0 && next < shut {
			depth++
			at += next + 1

			continue
		}

		depth--
		at += shut + len(name) + 3

		if depth == 0 {
			return at
		}
	}

	t.Fatalf("element <%s> opened at %d is never closed", name, open)

	return 0
}

// timeText blanks what the browser's own ticker rewrites: two renders seconds
// apart legitimately disagree about "4s ago", and that is not staleness.
var timeText = regexp.MustCompile(`<time[^>]*>[^<]*</time>`)

func TestNothingThatChangesLivesOutsideALiveRegion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		path   string
		setup  func(t *testing.T) (*Server, *Pipeline)
		change func(t *testing.T, pipeline *Pipeline)
	}{
		{
			// The reported bug: a run triggered while this page is open has to
			// show up in the history without a reload.
			name:  "job page",
			path:  "/p/demo/jobs/build",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
		{
			name:  "jobs board",
			path:  "/p/demo",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
		{
			name:  "runs page",
			path:  "/p/demo/runs",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
		{
			name:  "approvals page",
			path:  "/p/demo/approvals",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()

				_, err := pipeline.Store.RequestApproval(context.Background(), "build", "ship it?")
				if err != nil {
					t.Fatalf("RequestApproval: %v", err)
				}
			},
		},
		{
			name:  "questions page",
			path:  "/p/demo/questions",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)

				_, _, err := pipeline.Store.AskQuestion(context.Background(), store.Question{
					RunID: "run-1", JobName: "build", AgentName: "review",
					Question: "which branch?",
				})
				if err != nil {
					t.Fatalf("AskQuestion: %v", err)
				}
			},
		},
		{
			name:  "resources page",
			path:  "/p/demo/resources",
			setup: testPipeline,
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()

				err := pipeline.Store.RecordCheckedVersion(context.Background(), "repo", `{"ref":"abc123"}`)
				if err != nil {
					t.Fatalf("RecordCheckedVersion: %v", err)
				}
			},
		},
		{
			name: "overview",
			path: "/",
			setup: func(t *testing.T) (*Server, *Pipeline) {
				t.Helper()

				server, pipelines := testPipelines(t, "app", "infra")

				return server, pipelines[0]
			},
			change: func(t *testing.T, pipeline *Pipeline) {
				t.Helper()
				startRunningBuild(t, pipeline)
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, pipeline := test.setup(t)

			_, before := get(t, server, test.path)
			test.change(t, pipeline)
			_, after := get(t, server, test.path)

			assertChangesAreLive(t, test.path, before, after)
		})
	}
}

// assertChangesAreLive fails for every line the second render added that the
// refresh would not reach.
func assertChangesAreLive(t *testing.T, path, before, after string) {
	t.Helper()

	was := map[string]bool{}
	for _, line := range strings.Split(timeText.ReplaceAllString(before, "<time/>"), "\n") {
		was[strings.TrimSpace(line)] = true
	}

	regions := liveRegions(t, after)
	fresh := 0
	at := 0

	for _, line := range strings.Split(after, "\n") {
		start := at
		at += len(line) + 1

		if was[strings.TrimSpace(timeText.ReplaceAllString(line, "<time/>"))] {
			continue
		}

		fresh++

		if !within(regions, start) {
			t.Errorf("this line changed but sits outside every region htmx refreshes,\n"+
				"so a reader only sees it by reloading %s:\n\t%s", path, strings.TrimSpace(line))
		}
	}

	if fresh == 0 {
		t.Fatal("the state change did not alter this page at all — the probe proves nothing")
	}

	// Something on the page has to actually ASK. Every id above could be
	// named by an hx-select on a page whose poller sits in the chrome, or on
	// no element at all — regions nothing polls are regions nothing refreshes.
	if !strings.Contains(after, "hx-trigger=") {
		t.Error("no hx-trigger on this page, so it never polls at all")
	}
}

func within(spans [][2]int, at int) bool {
	for _, span := range spans {
		if at >= span[0] && at < span[1] {
			return true
		}
	}

	return false
}

// startRunningBuild leaves the run RUNNING, which is the state a page open
// across a trigger has to show.
func startRunningBuild(t *testing.T, pipeline *Pipeline) {
	t.Helper()

	err := pipeline.Store.StartRun(context.Background(), "run-1", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
}

// TestLiveRegionsAreDrivenByHtmx: the attributes above are inert markup
// unless the library that reads them is actually on the page and actually
// served, which is the seam a template-only assertion never crosses.
//
// The two modifiers are load-bearing and neither is htmx's default. outerMorph
// reconciles the existing DOM instead of replacing it, which is what lets the
// approvals and questions regions — forms somebody is typing a reason into —
// be refreshed at all; a plain outerHTML swap empties the input every 2.5
// seconds. The [!document.hidden] filter is what stops a forgotten background
// tab polling this server forever. Overlapping polls need no guard of ours:
// htmx's hx-sync defaults to `drop`, so a poll issued while one is in flight
// is ignored rather than queued behind it.
func TestLiveRegionsAreDrivenByHtmx(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/runs")

	if !strings.Contains(body, `<script src="/static/htmx.min.js"`) {
		t.Error("the page carries hx- attributes but never loads htmx")
	}

	for want, missing := range map[string]string{
		`hx-swap="outerMorph"`:                       "morph swap, so a swap empties the form under a reader",
		`hx-trigger="every 2.5s [!document.hidden]"`: "hidden-tab filter, so a forgotten tab polls forever",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("live region has no %s", missing)
		}
	}

	code, script := get(t, server, "/static/htmx.min.js")
	if code != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js = %d — the library is referenced but not served", code)
	}

	if !strings.Contains(script, "hx-select-oob") {
		t.Error("the served htmx does not know hx-select-oob, which every page depends on")
	}
}
